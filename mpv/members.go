package mpv

import "encoding/json"

// The notices the transports send, around a member's name (Dropped is the mesh's, before
// "<address> (<name>)"). apply pauses the party on HasLeft, and logs a member line for each
// (memberLine); e2e spells them again.
const (
	HasJoined   = " has joined"
	HasLeft     = " has left"
	ConnectedTo = "connected to "
	Dropped     = "dropped "
)

// memberLine is the log line, for the GUI and its history, saying who joined, was already
// here or left. It is made only from a notice the transport sent about its own sender, and
// names the member by the id the relay or the mesh gave it, so a name can't fake a member or
// stand for another with the same name.
func memberLine(text string, e IncomingMessage) (string, bool) {
	what := ""
	switch text {
	case e.HostName + HasJoined:
		what = "joined"
	case ConnectedTo + e.HostName:
		what = "here"
	case e.HostName + HasLeft, Dropped + e.From + " (" + e.HostName + ")":
		what = "left"
	default:
		return "", false
	}
	return "member " + what + ": " + memberOf(e), true
}

// memberOf is the member a message is from, as JSON, since a name can hold anything.
func memberOf(e IncomingMessage) string {
	b, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{e.From, e.HostName})
	return string(b)
}
