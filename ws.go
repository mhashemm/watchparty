package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mhashemm/watchparty/mpv"
)

const (
	secretHeaderKey = "room-secret"

	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second
)

// declared again in relay/main.go; both are package main so neither can import the
// other, and four lines beat a package that exists only to share them
type message struct {
	Hostname string          `json:"hostname"`
	Event    json.RawMessage `json:"event"`
}

type relayConn struct {
	mu   sync.Mutex
	conn *websocket.Conn

	addr     string
	room     string
	secret   string
	hostname string
	incoming chan<- mpv.IncomingMessage
}

func (r *relayConn) set(conn *websocket.Conn) {
	r.mu.Lock()
	r.conn = conn
	r.mu.Unlock()
}

// errRejected means the relay will never accept these credentials, so retrying
// only burns its rate limiter.
var errRejected = errors.New("rejected by relay")

func (r *relayConn) dial(c context.Context) (*websocket.Conn, error) {
	q := url.Values{"room": {r.room}, "hostname": {r.hostname}}
	conn, res, err := websocket.Dial(c, r.addr+"/ws?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{secretHeaderKey: {r.secret}},
	})
	if err != nil && res != nil {
		switch res.StatusCode {
		case http.StatusForbidden, http.StatusBadRequest:
			return nil, fmt.Errorf("%w: %s", errRejected, res.Status)
		}
	}
	return conn, err
}

func (r *relayConn) read(c context.Context, conn *websocket.Conn) error {
	for {
		_, b, err := conn.Read(c)
		if err != nil {
			return err
		}
		msg := message{}
		err = json.Unmarshal(b, &msg)
		if err != nil {
			log.Printf("relay: unmarshal: %s: %s\n", err, b)
			continue
		}
		select {
		case r.incoming <- mpv.IncomingMessage{HostName: msg.Hostname, Event: msg.Event}:
		case <-c.Done():
			return c.Err()
		}
	}
}

// Connect owns the reconnect loop for the lifetime of the process. A dropped
// relay is not a reason to tear down the player, so only a rejection cancels.
func (r *relayConn) Connect(c context.Context, cancel context.CancelFunc) {
	backoff := minBackoff
	for c.Err() == nil {
		conn, err := r.dial(c)
		if errors.Is(err, errRejected) {
			log.Printf("relay: %s; check -room and -secret\n", err)
			cancel()
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
		backoff = minBackoff
		r.set(conn)
		log.Printf("relay: connected to %s room %s\n", r.addr, r.room)

		err = r.read(c, conn)
		r.set(nil)
		conn.CloseNow()
		if c.Err() != nil {
			return
		}
		log.Printf("relay: disconnected: %s\n", err)
	}
}

// Broadcast is the single writer for the whole process. It must not be restarted
// per connection: two of these ranging over outgoing would steal frames from each
// other during a redial.
func (r *relayConn) Broadcast(c context.Context, outgoing <-chan []byte) {
	for event := range outgoing {
		b, err := json.Marshal(message{Hostname: r.hostname, Event: event})
		if err != nil {
			log.Printf("relay: marshal: %s\n", err)
			continue
		}
		// never hold mu across the write: it would wedge the supervisor's set() for
		// the whole timeout. mu only guards the pointer, and this is the only writer.
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
			// the supervisor is already redialling; the party resyncs on the next event
			log.Printf("relay: write: %s\n", err)
		}
	}
}

func (r *relayConn) Close() {
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn != nil {
		conn.Close(websocket.StatusNormalClosure, "bye")
	}
}
