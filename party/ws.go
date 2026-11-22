package party

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mhashemm/watchparty/mpv"
	"github.com/mhashemm/watchparty/relay/wire"
	"github.com/mhashemm/watchparty/server"
)

const (
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second
	notOpenFor = 30 * time.Minute
)

type relayConn struct {
	mu   sync.Mutex
	conn *websocket.Conn

	addr      string
	room      string
	secret    string
	createKey string
	creating  bool
	hostname  string
	id        string
	session   string
	file      string
	video     string
	incoming  chan<- mpv.IncomingMessage
	retry     time.Duration
	wait      time.Duration
	waiting   time.Time
	opened    bool
	up        chan struct{}
}

func (r *relayConn) set(conn *websocket.Conn) {
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
	if conn != nil {
		select {
		case r.up <- struct{}{}:
		default:
		}
	}
}

// errRejected means the relay will never accept these credentials, so retrying
// only burns its rate limiter.
var errRejected = errors.New("rejected by relay")

// dial returns a connection in the room. A keyless member of a room the relay remembers
// is held until someone with the key opens it, then dials again at once and joins.
func (r *relayConn) dial(c context.Context) (*websocket.Conn, error) {
	for {
		conn, held, err := r.dialOnce(c)
		if !held {
			return conn, err
		}
		log.Printf("relay: dial: room %s is not open yet, waiting for someone with -createKey\n", r.room)
		_, _, err = conn.Read(c)
		conn.CloseNow()
		switch websocket.CloseStatus(err) {
		case wire.StatusRoomOpen:
			continue
		case wire.StatusNotOpened:
			return nil, fmt.Errorf("%w: room %s did not open while the relay held this node", errRejected, r.room)
		}
		return nil, fmt.Errorf("waiting for room %s: %w", r.room, err)
	}
}

// dialOnce dials the relay once. held: the relay is holding this node until the room opens.
func (r *relayConn) dialOnce(c context.Context) (*websocket.Conn, bool, error) {
	q := url.Values{wire.RoomParam: {r.room}, wire.HostnameParam: {r.hostname}, wire.IDParam: {r.id}, wire.SessionParam: {r.session}, wire.FileParam: {r.file}}
	conn, res, err := websocket.Dial(c, strings.TrimRight(r.addr, "/")+wire.Path+"?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{wire.SecretHeader: {r.secret}, wire.CreateKeyHeader: {r.createKey}, wire.VideoHeader: {r.video}},
	})
	if err != nil && res != nil {
		switch res.StatusCode {
		case http.StatusForbidden, http.StatusBadRequest:
			return nil, false, fmt.Errorf("%w: %s; check -room and -secret", errRejected, res.Status)
		case http.StatusConflict:
			return nil, false, fmt.Errorf("%w: %s: %w", errRejected, res.Status, &server.DifferentFile{Video: res.Header.Get(wire.VideoHeader)})
		case http.StatusUnauthorized:
			if r.creating {
				return nil, false, fmt.Errorf("%w: %s; this relay needs -createKey to open a new room", errRejected, res.Status)
			}
			// a room nobody opens (a fake invite, a host who never comes) must not keep a
			// node waiting forever
			if r.waiting.IsZero() {
				r.waiting = time.Now()
			}
			if time.Since(r.waiting) >= r.wait {
				return nil, false, fmt.Errorf("%w: %s; room %s did not open within %s", errRejected, res.Status, r.room, r.wait)
			}
			return nil, false, fmt.Errorf("room %s is not open yet, waiting for someone with -createKey", r.room)
		// both clear by themselves, so the node keeps retrying; the status comes first for the GUI
		case http.StatusTooManyRequests:
			return nil, false, fmt.Errorf("%s: the relay has had too many attempts from your network, which clears within the hour", res.Status)
		case http.StatusServiceUnavailable:
			return nil, false, fmt.Errorf("%s: the relay is full", res.Status)
		}
	}
	if err == nil && res.Header.Get(wire.WaitingHeader) == "yes" {
		return conn, true, nil
	}
	// before the first frame, so mpv knows it opened the room before anyone joins
	r.opened = err == nil && res.Header.Get(wire.OpenedHeader) == "yes"
	if r.opened {
		r.tell(c, mpv.IncomingMessage{Opened: true})
	}
	return conn, false, err
}

// tell hands mpv news only this node's relay client knows, never anything from the wire.
func (r *relayConn) tell(c context.Context, m mpv.IncomingMessage) {
	select {
	case r.incoming <- m:
	case <-c.Done():
	}
}

// read hands the room's messages to mpv until the connection fails.
func (r *relayConn) read(c context.Context, conn *websocket.Conn) error {
	for {
		// not c: when it ends, the library would close the conn before the goodbye is sent
		_, b, err := conn.Read(context.WithoutCancel(c))
		if err != nil {
			return err
		}
		msg := wire.Message{}
		err = json.Unmarshal(b, &msg)
		if err != nil {
			// not b: it may carry a chat message, which no log may keep (mpv/chat.go)
			log.Printf("relay: unmarshal: %s (%d bytes)\n", err, len(b))
			continue
		}
		select {
		case r.incoming <- mpv.IncomingMessage{HostName: msg.Hostname, From: msg.From, Event: msg.Event}:
		case <-c.Done():
			return c.Err()
		}
	}
}

// Connect dials and redials the relay for the whole party. A dropped connection never ends
// the party; only a rejection stops it, as the cause, which Run returns.
//
// mpv is told when the connection is lost (Lost), and plays on, and when it is back (Back),
// so it tells the party where it is: what it sent meanwhile was dropped, and what the party
// sent never arrived. Not Back if the redial opened the room: nobody is there yet, and this
// node answers the others.
func (r *relayConn) Connect(c context.Context, stop context.CancelCauseFunc) {
	backoff, dropped := r.retry, false
	for c.Err() == nil {
		conn, err := r.dial(c)
		// Run returns it, and the CLI prints it or the GUI diagnoses it, once
		if errors.Is(err, errRejected) {
			stop(err)
			return
		}
		if err != nil {
			log.Printf("relay: dial: %s, retrying in %s\n", err, backoff)
			select {
			case <-time.After(backoff):
			case <-c.Done():
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff, r.waiting = r.retry, time.Time{}
		r.set(conn)
		log.Printf("relay: connected to %s room %s\n", r.addr, r.room)
		if dropped && !r.opened {
			r.tell(c, mpv.IncomingMessage{Back: true})
		}

		// when the party ends, say goodbye, so the relay tells the room at once instead of
		// waiting for this node to come back
		bye := context.AfterFunc(c, func() { conn.Close(websocket.StatusNormalClosure, "bye") })
		err = r.read(c, conn)
		bye()
		r.set(nil)
		conn.CloseNow()
		if c.Err() != nil {
			return
		}
		log.Printf("relay: disconnected: %s\n", err)
		dropped = true
		r.tell(c, mpv.IncomingMessage{Lost: true})
	}
}

// Broadcast is the only writer for the whole party. It must not be restarted per
// connection: two of them reading outgoing would steal frames from each other during a
// redial.
//
// It starts once the first dial has succeeded: mpv can finish loading before that, and a
// "joined" sent nowhere would leave the joiner at 0 while the party plays on.
func (r *relayConn) Broadcast(c context.Context, outgoing <-chan []byte) {
	select {
	case <-r.up:
	case <-c.Done():
		return
	}
	for {
		var event []byte
		select {
		case event = <-outgoing:
		case <-c.Done():
			return
		}
		b, err := json.Marshal(wire.Message{Hostname: r.hostname, From: r.id, Event: event})
		if err != nil {
			log.Printf("relay: marshal: %s\n", err)
			continue
		}
		// never hold mu across the write: it would block Connect's set() for the whole
		// timeout. mu only guards the pointer, and this is the only writer.
		r.mu.Lock()
		conn := r.conn
		r.mu.Unlock()
		if conn == nil {
			log.Println("relay: not connected, dropping event")
			continue
		}
		ctx, cancel := context.WithTimeout(c, 10*time.Second)
		err = conn.Write(ctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			// Connect is already redialling, and tells the party where it is once back
			log.Printf("relay: write: %s\n", err)
		}
	}
}
