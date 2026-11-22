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

// do sends mpv one command and waits for its answer. It runs on the loop, the node's one
// goroutine, and keeps handling inputs while it waits: mpv's lines are handled, a transport
// message waits its turn (handle), and other requests' answers are kept for the do() waiting
// on them. Never call it from another goroutine.
func (c *Client) do(args ...any) error {
	_, err := c.ask(args...)
	return err
}

// ask is do, and also returns the answer's data, such as get_property_string's value.
func (c *Client) ask(args ...any) (string, error) {
	return c.send(false, args)
}

// send is ask; an unlogged request never reaches the log, as a chat message must not
// (chat.go).
func (c *Client) send(unlogged bool, args []any) (string, error) {
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
	c.out.put(request{id: id, body: body, unlogged: unlogged})
	return c.await(id, replyLimit)
}

// await handles inputs on the loop until id is answered, for at most limit: mpv's reply to
// a request, or the script's ack of a set (setState).
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

// mailbox is an unbounded FIFO. put never blocks, so mpv's lines keep being read while the
// loop writes to mpv: otherwise the reader waits on the loop, the loop on mpv, and mpv on
// the reader, forever.
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
	id       int64
	body     []byte
	unlogged bool
}

// writer writes requests to mpv's socket in order, so the loop never blocks on mpv reading
// it: a stalled mpv would make every peer wait. A failed write comes back as an input.
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
		if !r.unlogged {
			log.Printf("do: sent request: %s\n", r.body)
		}
	}
}

// outcome is mpv's answer to one request, or why there is none.
type outcome struct {
	Response
	data    string
	expired bool
	unsent  error
}

// input is one thing for the loop to handle: a line from mpv, mpv's socket closing, a
// request timing out or failing to write, or a transport message.
type input struct {
	line    []byte
	closed  bool
	expired int64
	unsent  int64
	err     error
	msg     *IncomingMessage
	// chatDue: a chat message on mpv's screen is due to go
	chatDue bool
}

// watch runs the loop until mpv's socket closes. A reader goroutine posts mpv's lines in
// order; the loop handles them and every other input, one at a time.
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

// serve keeps handling inputs after mpv's socket closed, until the party ends, so a peer
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

// next returns the loop's next input. Transport messages come first: the transport has
// already let go of them, so they came before anything mpv says from here, and a message
// sent while the file loads is heard while it loads. incoming is read here, on the loop,
// never by a goroutine that would hold a message before passing it on.
func (c *Client) next() (input, bool) {
	for {
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		case text := <-c.chat:
			return input{msg: &IncomingMessage{typed: true, say: text}}, true
		default:
		}
		if in, ok := c.inbox.poll(); ok {
			return in, true
		}
		select {
		case m := <-c.incoming:
			return input{msg: &m}, true
		case text := <-c.chat:
			return input{msg: &IncomingMessage{typed: true, say: text}}, true
		case <-c.inbox.ready:
		case <-c.ctx.Done():
			return input{}, false
		}
	}
}

// handle handles one input and whatever it starts. A transport message is applied only
// when no other one is still being applied, so each is applied fully, through mpv's
// answers, before the next; mpv's lines are handled in between.
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
	case in.chatDue:
		c.later = append(c.later, func() {
			if err := c.chatGoes(); err != nil {
				log.Println("chat:", err)
			}
		})
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
	// the party's state is applied through the script, so joining also waits for its name
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

// runLater runs the commands inputs started (later), unless a set is pending.
func (c *Client) runLater() {
	for len(c.later) > 0 && c.pending == nil {
		f := c.later[0]
		c.later = c.later[1:]
		f()
	}
}

// mpvSaid handles one line from mpv: an answer to a request, or an event.
func (c *Client) mpvSaid(line []byte) {
	event := Event{}
	if err := json.Unmarshal(line, &event); err != nil {
		// an answer whose data isn't a string, like osd-overlay's {}, still answers its request
		r := Response{}
		if json.Unmarshal(line, &r) == nil && r.RequestId > 0 && r.Error != "" && c.waiting[r.RequestId] {
			c.replies[r.RequestId] = outcome{Response: r}
			return
		}
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

// say sends the party one message.
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
