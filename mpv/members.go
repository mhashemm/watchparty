package mpv

import "encoding/json"

// The notices the transports show, around a member's name (Dropped: the mesh's, before
// "<address> (<name>)"). apply reads HasLeft to pause the party, and logs a member line for
// each (memberLine); e2e spells them again.
const (
	HasJoined   = " has joined"
	HasLeft     = " has left"
	ConnectedTo = "connected to "
	Dropped     = "dropped "
)

// memberLine is who joined, was here first or left, for the GUI and its history: only from a
// notice the transport sent about its own sender, named by the id the relay or the mesh gave
// it, so a name can't fake a member nor stand for another with the same name.
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

// memberOf is the member a message is from, as JSON: its name can hold anything.
func memberOf(e IncomingMessage) string {
	b, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{e.From, e.HostName})
	return string(b)
}
