package mpv

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
)

// ahead tells the party how many whole seconds this node has ready, after it ran out of
// cache on a stream and while the party waits for it. After sending a number, it waits that
// many seconds before the next, between reportMin and reportMax: a slow member costs the
// party one tiny message a minute once it is well on its way, and nothing while it plays or
// on a local file.
func (c *Client) ahead(data string) {
	secs, err := strconv.ParseFloat(data, 64)
	if !c.slowStream || !c.paused || err != nil || int(secs) == c.readySent || time.Now().Before(c.reportAt) {
		return
	}
	c.readySent = int(secs)
	c.reportAt = time.Now().Add(min(max(time.Duration(secs*float64(time.Second)), reportMin), reportMax))
	log.Printf("buffered ahead: %d\n", c.readySent)
	b, _ := json.Marshal(Event{Name: buffered, Data: strconv.Itoa(c.readySent)})
	c.say(b)
	// on mpv's screen too, now and then: people watch mpv, not the app
	if louder(c.readySent, c.readyOnScreen) {
		c.readyOnScreen = c.readySent
		text := fmt.Sprintf("your network is slow: %ds ready", c.readySent)
		c.later = append(c.later, func() { c.showText(text) })
	}
}

// louder is whether secs ready should go on mpv's screen, given the last shown (-1: none).
func louder(secs, shown int) bool {
	return shown < 0 || secs >= 2*max(shown, screenFrom)
}
