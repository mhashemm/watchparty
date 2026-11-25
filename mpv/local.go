package mpv

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"
)

// handleEvent returns what to broadcast for one line from mpv: the line itself if it is this
// node's own change, then anything that must follow it. Everything is tagged here, in the
// step that decided it goes out, so it carries the stamp that change was given.
func (c *Client) handleEvent(event Event, line []byte) [][]byte {
	// log mpv's lines to read their order after a run, except the noisy ones: time-pos
	// while playing (one per frame), cache fill (the buffering line says it), cache ahead
	// (a few times a second)
	if event.Name != timePos && event.Name != cacheFill && event.Name != cacheAhead || event.Name == timePos && c.paused {
		log.Printf("Watch: mpv said: %s\n", line)
	}
	var out [][]byte
	if event.EventType == propertyChange && event.Data != "" {
		var send bool
		if out, send = c.propertyChanged(event); send {
			out = append([][]byte{line}, out...)
		}
	}
	if len(out) > 0 {
		// our own change takes the next stamp. Only the person's own pause, seek, move or speed
		// leads the stop; any other stop is leaderless, and its first seek leads.
		lead := event.Name == timePos || event.Name == playSpeed || event.Name == pause && event.paused() && !c.pausedByNode ||
			event.Name == playlistPos && !c.advancedAtEnd
		c.stamp, c.leaderID, c.leaderless = c.stamp+1, "", !lead
	}
	if event.Name == pause {
		c.pausedByNode = false
	}
	for i, b := range out {
		out[i] = c.tag(b)
	}
	return out
}

// propertyChanged decides what a property change means for the party: whether the event
// itself goes out (send), and what to broadcast after it or instead of it.
func (c *Client) propertyChanged(event Event) (extra [][]byte, send bool) {
	extra, moved := c.record(event)
	first := !c.seen[event.Name]
	c.seen[event.Name] = true
	if first && event.Name == timePos {
		close(c.ready)
	}
	switch event.Name {
	case pause:
		extra, send = c.pauseChanged(event.paused(), extra)
	case timePos:
		if !event.fromScript {
			// mpv's playing position; where it stops and lands comes from the script
			return nil, false
		}
		send = c.positionChanged()
	case pausedForCache:
		was := c.waitingOnCache
		c.waitingOnCache = event.paused()
		if c.waitingOnCache && c.onNetwork && !c.slowStream {
			c.slowStream, c.readySent, c.readyOnScreen, c.reportAt = true, -1, -1, time.Time{}
		}
		if c.waitingOnCache && !c.paused {
			c.pauseParty("buffering", false)
		}
		if was && !c.waitingOnCache {
			log.Println("buffering: done")
		}
		return nil, false
	case viaNetwork:
		c.onNetwork = event.Data == "yes"
		return nil, false
	case playSpeed:
		if !event.fromScript {
			return nil, false
		}
		return nil, c.speedChanged(event.Data)
	case cacheAhead:
		c.ahead(event.Data)
		return nil, false
	case cacheFill:
		// it reads 100 except while mpv waits on its cache, when it climbs a few times a second
		if c.waitingOnCache {
			log.Printf("buffering: %s%%\n", event.Data)
		}
		return nil, false
	case playlistPos:
		if !event.fromScript {
			return nil, false
		}
		send = c.entryChanged(moved)
	default:
		return nil, false
	}
	if send && first {
		return nil, false
	}
	return extra, send
}

// record stores what mpv reported, whether or not it is an echo of our own change. It
// returns what a pause carries, and whether the playlist moved to another entry after the
// first file opened.
func (c *Client) record(event Event) (extra [][]byte, moved bool) {
	switch event.Name {
	case timePos:
		// while paused, mpv is where the script last said: a playing second can arrive
		// after the pause (MPV.md, [R6c]). While a set is pending, mpv may not be where the
		// person took it yet.
		if pos, err := strconv.ParseFloat(event.Data, 64); err == nil {
			c.reported = pos
			if event.fromScript || !c.paused && c.pending == nil {
				c.pos = pos
			}
		}
	case duration:
		if d, err := strconv.ParseFloat(event.Data, 64); err == nil {
			c.duration = d
		}
	case pause:
		if event.paused() {
			extra = c.pauseExtras()
		}
	case filename:
		name, _ := json.Marshal(event.Data)
		log.Printf("now playing: %s\n", name)
	case playlistPos:
		if n, err := strconv.Atoi(event.Data); err == nil && n >= 0 && n != c.current && event.fromScript {
			c.advancedAtEnd = c.lastStartAuto
			c.current = n
			moved = c.seen[timePos]
			c.clear(n)
			if event.stoppedAt != nil {
				c.pos = *event.stoppedAt
			}
		}
	}
	return extra, moved
}

// clear forgets the old second and our seeks for other files when mpv opens entry: the old
// second would go out with the next pause, and a seek queued for the old file never lands.
func (c *Client) clear(entry int) {
	c.pos, c.seekWhilePlaying = 0, false
	c.ourSeeks = slices.DeleteFunc(c.ourSeeks, func(s ourSeek) bool { return s.entry != entry })
	c.seekPending = c.seekPending && len(c.ourSeeks) > 0
}

// pauseChanged handles the person's own pause or resume.
func (c *Client) pauseChanged(paused bool, extra [][]byte) ([][]byte, bool) {
	if c.paused == paused {
		return extra, false
	}
	if !paused && c.waitingOnCache {
		c.pauseParty("still buffering", false)
		return nil, false
	}
	if !paused && c.led() && c.heading() != c.partyEntry {
		// a follower resuming on a file the person moved to. Like a move while playing, the
		// party stops there, on this node's second, and this node leads: everyone has to load
		// the file first, and would otherwise play far apart.
		c.paused = false
		c.pauseParty("video changed", true)
		return nil, false
	}
	c.paused = paused
	if !paused {
		c.seekWhilePlaying, c.resumedFrom = false, c.target()
		c.slowStream = false
		clear(c.peerReadyOnScreen)
	}
	return extra, true
}

// speedChanged handles the person changing mpv's speed, as a seek: while paused it goes out,
// a follower's staying on its screen; while playing it stops the party, as each node would
// take it at another moment, and the stop says it.
func (c *Client) speedChanged(data string) bool {
	v, err := strconv.ParseFloat(data, 64)
	if err != nil || v == c.speed {
		return false
	}
	c.speed = v
	if c.paused && c.led() {
		c.keepOnScreen()
		return false
	}
	if c.paused {
		return true
	}
	if c.stopReason == "" {
		c.pauseParty("speed changed", true)
	}
	return false
}

// positionChanged handles the person's seek, as the script reported where it landed
// (wentTo). While paused it goes out; while playing it pauses the party.
func (c *Client) positionChanged() bool {
	if c.paused && c.led() {
		// only the leader seeks for the party; the leader's next seek overrides this one
		c.keepOnScreen()
		return false
	}
	// the person's seek is newer than any of ours: mpv drops a seek queued before it, and
	// one sent after lands on the person's second too, so our marks are done.
	c.ourSeeks, c.seekPending = nil, false
	if c.paused {
		return true
	}
	if c.seekWhilePlaying {
		c.seekWhilePlaying = false
		c.pauseParty("seek", true)
	}
	return false
}

// keepOnScreen leaves a follower's seek or move on this screen only, until the party
// resumes. The person is told once per state taken, so a held key doesn't flood the feed.
func (c *Client) keepOnScreen() {
	if !c.toldWhoLeads {
		c.toldWhoLeads = true
		text := fmt.Sprintf("only %s can seek or change the speed until resumed", c.leaderName)
		c.later = append(c.later, func() { c.showText(text) })
	}
}

// entryChanged handles this node moving to another entry by itself: the move goes out, and
// pauses the party if it was playing. A follower's move stays on its screen, like its seek:
// a move is a seek into another file. mpv moving on at a file's end leads nothing.
func (c *Client) entryChanged(moved bool) bool {
	if !moved {
		return false
	}
	if c.led() {
		c.keepOnScreen()
		return false
	}
	if !c.paused {
		c.pauseParty("video changed", !c.advancedAtEnd)
	}
	return true
}

// pauseExtras is what follows a pause that pauseParty started: where mpv stopped, and why.
func (c *Client) pauseExtras() [][]byte {
	if c.stopReason == "" {
		return nil
	}
	reason, _ := json.Marshal(Event{Name: pauseReason, Data: c.stopReason})
	c.stopReason = ""
	return [][]byte{position(timePos, c.pos), reason}
}

func position(name string, pos float64) []byte {
	b, _ := json.Marshal(Event{EventType: propertyChange, Name: name, Data: seconds(pos)})
	return b
}

// seconds is a second as mpv and the script write it.
func seconds(pos float64) string {
	return strconv.FormatFloat(pos, 'f', 6, 64)
}

// pauseParty stops the party because this node is out of step. lead: the person's seek or
// move caused it, so this node leads the stop; otherwise it is leaderless.
func (c *Client) pauseParty(reason string, lead bool) {
	c.stopReason = reason
	c.later = append(c.later, func() {
		c.pausedByNode = !lead
		err := c.pauseReq(true)
		if err != nil {
			log.Println("pauseParty:", err)
			c.stopReason = ""
			return
		}
		c.showText("paused for everyone: " + reason)
	})
}
