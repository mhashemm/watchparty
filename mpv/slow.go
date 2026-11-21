package mpv

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"
)

// ahead tells the party how many whole seconds this node has ready, after it ran out of cache
// on a stream and while the party waits for it. Once it said a number, the next waits as many
// seconds, from reportMin to reportMax: a slow member costs the party a tiny message a minute
// once it has some way to go, and nothing at all while it plays or on a local file.
func (c *Client) ahead(data string) {
	secs, err := strconv.ParseFloat(data, 64)
	if !c.slow || !c.paused || err != nil || int(secs) == c.readySaid || time.Now().Before(c.reportAt) {
		return
	}
	c.readySaid = int(secs)
	c.reportAt = time.Now().Add(min(max(time.Duration(secs*float64(time.Second)), reportMin), reportMax))
	log.Printf("buffered ahead: %d\n", c.readySaid)
	b, _ := json.Marshal(Event{Name: buffered, Data: strconv.Itoa(c.readySaid)})
	c.say(b)
	// on the screen too, now and then: people watch in mpv, not in the app
	if louder(c.readySaid, c.readyShown) {
		c.readyShown = c.readySaid
		text := fmt.Sprintf("your network is slow: %ds ready", c.readySaid)
		c.later = append(c.later, func() { c.showText(text) })
	}
}

// louder says whether secs ready go on mpv's screen, the last shown being shown (-1: none).
func louder(secs, shown int) bool {
	return shown < 0 || secs >= 2*max(shown, screenFrom)
}
