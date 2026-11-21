package mpv

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"time"
)

// handleEvent returns what goes out for one line from mpv: the line itself when it is
// this node's own change, and whatever must follow it. Each is tagged in the step that
// decided it, so its stamp is the one that change was given.
func (c *Client) handleEvent(event Event, line []byte) [][]byte {
	// mpv's own order, to read after a run; not its position while playing, one per frame,
	// nor how full its cache is, which the buffering line says, nor how far ahead, a few
	// times a second
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
		// this node's own change, or what it adds to a peer's: the next stamp. Only the
		// person's own pause, seek or move leads; any other stop is free, and the first seek
		// leads.
		lead := event.Name == timePos || event.Name == pause && event.paused() && !c.pausedByNode ||
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

// propertyChanged is what a property-change with data means for the party: whether the event
// itself goes out (send), and what to broadcast after it, or instead of it.
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
			// how far mpv has played; where it stops and lands, the script says
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
	case cacheAhead:
		c.ahead(event.Data)
		return nil, false
	case cacheFill:
		// 100 but while mpv waits on its cache, when it climbs a few times a second
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

// record keeps what mpv reported, echo or not. It returns what a pause carries, and
// whether the playlist moved to another entry after the first file was open.
func (c *Client) record(event Event) (extra [][]byte, moved bool) {
	switch event.Name {
	case timePos:
		// stopped, mpv is where the script last said: a playing second it reported before
		// the stop can come after (MPV.md, [R6c]); and while a set is on its way, mpv may not
		// play yet, wherever the person took it
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

// clear forgets the second this node was on and the seeks it sent for other files, as mpv
// opens entry: the old second would go out with a pause, and a seek queued for the old file
// never lands.
func (c *Client) clear(entry int) {
	c.pos, c.seekWhilePlaying = 0, false
	c.ourSeeks = slices.DeleteFunc(c.ourSeeks, func(s ourSeek) bool { return s.entry != entry })
	c.seekPending = c.seekPending && len(c.ourSeeks) > 0
}

// pauseChanged is the user's own pause or resume.
func (c *Client) pauseChanged(paused bool, extra [][]byte) ([][]byte, bool) {
	if c.paused == paused {
		return extra, false
	}
	if !paused && c.waitingOnCache {
		c.pauseParty("still buffering", false)
		return nil, false
	}
	if !paused && c.led() && c.heading() != c.partyEntry {
		// a follower resuming on the file the person moved to: as a move while playing, the
		// party stops there, on this node's second, and it leads. Everyone must load the
		// file first, and they would play that far apart.
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

// positionChanged is the person's seek, as the script said where it went (wentTo): stopped, it
// goes out; playing, it pauses the party.
func (c *Client) positionChanged() bool {
	if c.paused && c.led() {
		// the step taking the leader's state goes on: its seek overrides this one
		c.keepOnScreen()
		return false
	}
	// the person's seek goes out, or stops the party, newer: wherever this node was being
	// sent, this is where it is now. mpv lands nothing of a seek queued before it, and one
	// sent after it lands as the person's, newer, which is where mpv then is.
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

// keepOnScreen keeps a follower's seek or move on this screen until the party resumes, and
// tells the person, once for each state it takes, so a held key doesn't flood the feed.
func (c *Client) keepOnScreen() {
	if !c.toldWhoLeads {
		c.toldWhoLeads = true
		text := fmt.Sprintf("only %s can seek until resumed", c.leaderName)
		c.later = append(c.later, func() { c.showText(text) })
	}
}

// entryChanged is this node moving to another entry by itself: it goes out, and pauses
// the party if it was playing. A follower's stays on its screen, as its seek does: a move is a
// seek far enough to land in another file. mpv moving on from the end is nobody's choice.
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

// pauseParty stops the party for why this node is out of step: led by it after the person's
// seek or move (lead), by nobody otherwise.
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
