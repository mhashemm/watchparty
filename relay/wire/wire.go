// Package wire is what a node and the relay say to each other. Both import it, so the
// two programs can't drift apart; it imports nothing that would grow the relay image.
package wire

import (
	"encoding/json"

	"github.com/coder/websocket"
)

// Path is where a node dials, with the room, hostname, session and file in the query.
// The secret and the create key go in headers, never the query: proxies log it.
const (
	Path = "/ws"

	RoomParam     = "room"
	HostnameParam = "hostname"
	SessionParam  = "session"
	FileParam     = "file"
	// WaitParam set to "1" says the node can be held until a key holder opens the room;
	// a node that doesn't send it gets 401 and retries, as it always did.
	WaitParam = "wait"

	SecretHeader    = "room-secret"
	CreateKeyHeader = "create-key"
	// OpenedHeader is "yes" on the 101 for the node whose request created the room.
	OpenedHeader = "room-opened"
	// WaitingHeader is "yes" on the 101 for a node held until a key holder opens the room.
	WaitingHeader = "room-waiting"
)

// How the relay closes a held node: join now, or give up.
const (
	StatusRoomOpen  websocket.StatusCode = 4001
	StatusNotOpened websocket.StatusCode = 4002
)

// Message is every frame: the sender's hostname and its raw mpv JSON, which the relay
// forwards without parsing.
type Message struct {
	Hostname string          `json:"hostname"`
	Event    json.RawMessage `json:"event"`
}
