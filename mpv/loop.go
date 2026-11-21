package mpv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

type connection struct {
	conn    net.Conn
	scanner *bufio.Scanner
	closed  chan struct{}
}

// do sends mpv one command and waits for its answer on the loop, the node's one goroutine:
// meanwhile it takes the inputs that come, as loop does, so mpv's lines are handled and a
// transport message waits its turn (handle). Any other request's answer is kept for the do()
// waiting on it. Never from another goroutine.
func (c *Client) do(args ...any) error {
	_, err := c.ask(args...)
	return err
}

// ask is do, and gives the answer's data: get_property_string's value.
func (c *Client) ask(args ...any) (string, error) {
	if c.gone {
		return "", ErrClosed
	}
	req := Request{
		Command:   args,
		RequestId: rand.Int63(),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	id := req.RequestId
	if c.out == nil {
		c.out = newMailbox[request]()
		go c.writer(c.out)
	}
	c.out.put(request{id: id, body: body})
	return c.await(id, replyLimit)
}

// await waits on the loop for id's answer, taking the inputs that come meanwhile: mpv's reply
// to a request, or the script's ack of a set (setState), for at most limit.
func (c *Client) await(id int64, limit time.Duration) (string, error) {
	c.waiting[id] = true
	defer delete(c.waiting, id)
	expire := time.AfterFunc(limit, func() { c.post(input{expired: id}) })
	defer expire.Stop()
	for {
		if r, ok := c.replies[id]; ok {
			delete(c.replies, id)
			if r.unsent != nil {
				return "", fmt.Errorf("%w: %v", ErrClosed, r.unsent)
			}
			if r.expired {
				log.Printf("do: response timeout: [requestId=%d]\n", id)
				return "", errTimeout
			}
			log.Printf("do: received response: [requestId=%d] [error=%s]\n", id, r.Error)
			if r.Error != "success" {
				return "", errors.New(r.Error)
			}
			return r.data, nil
		}
		if c.gone {
			return "", ErrClosed
		}
		in, ok := c.next()
		if !ok {
			return "", ErrClosed
		}
		c.handle(in)
	}
}

// mailbox is the loop's inbox: what it is given, in the order given. Giving never waits,
// so mpv's lines are read while the loop writes to mpv: a reader waiting on the loop while
// the loop waits on mpv, and mpv on the reader, would wait forever.
type mailbox[T any] struct {
	mu    sync.Mutex
	items []T
	ready chan struct{}
}

func newMailbox[T any]() *mailbox[T] {
	return &mailbox[T]{ready: make(chan struct{}, 1)}
}

func (m *mailbox[T]) put(v T) {
	m.mu.Lock()
	m.items = append(m.items, v)
	m.mu.Unlock()
	select {
	case m.ready <- struct{}{}:
	default:
	}
}

func (m *mailbox[T]) poll() (T, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var v T
	if len(m.items) == 0 {
		return v, false
	}
	v = m.items[0]
	m.items = m.items[1:]
	return v, true
}

// request is one command on its way to mpv's socket.
type request struct {
	id   int64
	body []byte
}

// writer writes requests to mpv's socket in order, so the loop never waits on mpv reading
// it: a stalled mpv would make every peer wait. A write that fails comes back as an input.
func (c *Client) writer(out *mailbox[request]) {
	for {
		r, ok := out.poll()
		if !ok {
			select {
			case <-out.ready:
				continue
			case <-c.ctx.Done():
				return
			}
		}
		if _, err := c.conn.conn.Write(append(r.body, '\n')); err != nil {
			c.post(input{unsent: r.id, err: err})
			continue
		}
		log.Printf("do: sent request: %s\n", r.body)
	}
}

// outcome is mpv's answer to one request, or none in 5s.
type outcome struct {
	Response
	data    string
	expired bool
	unsent  error
}

// input is one thing that happened to this node: a line mpv sent, mpv's socket closing, a
// request's 5s running out or its write failing, or a transport message.
type input struct {
	line    []byte
	closed  bool
	expired int64
	unsent  int64
	err     error
	msg     *IncomingMessage
}

// watch runs the loop until mpv's socket closes: a reader hands it mpv's lines in order,
// and it handles them and everything else that comes, one at a time.
func (c *Client) watch() error {
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := c.conn.scanner
		for scanner.Scan() {
			// the scanner reuses its buffer on the next Scan
			if !c.post(input{line: bytes.Clone(scanner.Bytes())}) {
				return
			}
		}
		err = scanner.Err()
		c.post(input{closed: true})
	}()
	c.runLater()
	for !c.gone {
		in, ok := c.next()
		if !ok {
			return c.ctx.Err()
		}
		c.handle(in)
	}
	<-done
	return err
}

// serve keeps taking inputs after mpv's socket closed, until the party ends: a peer still
// never waits on this node.
func (c *Client) serve() {
	for {
		in, ok := c.next()
		if !ok {
			return
		}
		c.handle(in)
	}
}

func (c *Client) post(in input) bool {
	if c.ctx.Err() != nil {
		return false
	}
	c.inbox.put(in)
	return true
}

// next is the loop's next input. What the transport has handed over comes first: its
// sender is done with it, so it came before whatever mpv says from here, and a message sent
// before the file loaded is heard while it loads. Read on the loop itself, never by a
// goroutine that would hold it a moment before passing it on.
func (c *Client) next() (input, bool) {
	for {
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		default:
		}
		if in, ok := c.inbox.poll(); ok {
			return in, true
		}
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		case <-c.inbox.ready:
		case <-c.ctx.Done():
			return input{}, false
		}
	}
}

// handle is one input, with whatever it starts. The only rule for order: a transport
// message is taken only when no other's change is still being applied, so each is applied
// to its end, as mpv answers it, before the next; mpv's lines are handled meanwhile.
func (c *Client) handle(in input) {
	switch {
	case in.closed:
		c.gone = true
		if c.opening {
			c.open(ErrClosed)
		}
	case in.expired != 0:
		if c.waiting[in.expired] {
			c.replies[in.expired] = outcome{expired: true}
		}
	case in.unsent != 0:
		if c.waiting[in.unsent] {
			c.replies[in.unsent] = outcome{unsent: in.err}
		}
	case in.msg != nil:
		if c.opening {
			c.hear(*in.msg)
		} else {
			c.queue = append(c.queue, *in.msg)
		}
	default:
		c.mpvSaid(in.line)
	}
	c.runLater()
	// the party's state is applied through the script, so it waits for its name too
	if c.opening && c.seen[timePos] && c.script != "" {
		c.join()
	}
	for c.applying == 0 && len(c.queue) > 0 {
		c.applying++
		m := c.queue[0]
		c.queue = c.queue[1:]
		c.apply(m)
		c.applying--
		c.runLater()
	}
	c.snapshot()
}

func (c *Client) runLater() {
	for len(c.later) > 0 && c.pending == nil {
		f := c.later[0]
		c.later = c.later[1:]
		f()
	}
}

// mpvSaid is one line from mpv: an answer to a request, or an event.
func (c *Client) mpvSaid(line []byte) {
	event := Event{}
	if err := json.Unmarshal(line, &event); err != nil {
		log.Println("Watch: unmarshal:", err, string(line))
		return
	}
	if event.RequestId > 0 && event.Error != "" {
		if c.waiting[event.RequestId] {
			c.replies[event.RequestId] = outcome{Response: event.Response, data: event.Data}
			log.Println("Watch: queued response:", string(line))
		} else {
			log.Println("Watch: dropped response:", string(line))
		}
	}
	if event.EventType == clientMessage {
		log.Printf("Watch: mpv said: %s\n", line)
		for _, report := range c.scriptSaid(event.Args) {
			line, _ := json.Marshal(report)
			c.broadcast(c.handleEvent(report, line))
		}
		return
	}
	if event.EventType == "" {
		return
	}
	c.broadcast(c.handleEvent(event, line))
}

func (c *Client) broadcast(out [][]byte) {
	for _, b := range out {
		select {
		case c.outgoing <- b:
			log.Printf("Watch: broadcasted event: %s\n", b)
		case <-c.ctx.Done():
			return
		}
	}
}

// say sends the party one message of this node's own.
func (c *Client) say(b []byte) {
	select {
	case c.outgoing <- b:
	case <-c.ctx.Done():
	}
}

func (c *Client) reply(to chan<- []byte, b []byte) bool {
	select {
	case to <- b:
		return true
	case <-c.ctx.Done():
		return false
	}
}
