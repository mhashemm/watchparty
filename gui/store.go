package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mhashemm/watchparty/mpv"
)

const (
	maxHistory = 100
	maxVideos  = 200
)

type settings struct {
	Hostname  string `json:"hostname"`
	Relay     string `json:"relay"`
	Mpv       string `json:"mpv"`
	MpvFlags  string `json:"mpvFlags"`
	CreateKey string `json:"createKey"`
	CacheDir  string `json:"cacheDir"`
	PPort     string `json:"pport"`
	Local     bool   `json:"local"`
	Port      string `json:"port"`
}

type session struct {
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	File     string    `json:"file"`
	Mode     string    `json:"mode"`
	Relay    string    `json:"relay,omitempty"`
	Room     string    `json:"room,omitempty"`
	Playlist bool      `json:"playlist,omitempty"`
	Secret   string    `json:"secret,omitempty"`
	Addrs    string    `json:"addrs,omitempty"`
	Invite   string    `json:"invite,omitempty"`
	Finished bool      `json:"finished,omitempty"`
	Stopped  stop      `json:"stopped,omitzero"`
	People   []string  `json:"people"`
	Videos   []played  `json:"videos,omitempty"`
}

type played struct {
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
}

// stop is where Continue starts: a playlist's entry, by name, and the second into it.
type stop struct {
	Entry string  `json:"entry,omitempty"`
	At    float64 `json:"at,omitempty"`
}

func (sess *session) openVideo(name string, at time.Time) {
	sess.finishVideo(at)
	sess.Videos = append(sess.Videos, played{Name: name, Started: at})
	if over := len(sess.Videos) - maxVideos; over > 0 {
		sess.Videos = slices.Delete(sess.Videos, 0, over)
	}
}

func (sess *session) finishVideo(at time.Time) {
	if n := len(sess.Videos); n > 0 {
		sess.Videos[n-1].Ended = at
	}
}

type store struct {
	path        string
	historyPath string

	mu       sync.Mutex
	Settings settings  `json:"settings"`
	History  []session `json:"history"`
}

func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "watchparty", "gui.json")
}

func openStore(path string) *store {
	s := &store{path: path, historyPath: filepath.Join(filepath.Dir(path), "history.json"), Settings: settings{Local: true}}
	b, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal(b, s)
	}
	inSettings := len(s.History) > 0
	b, err = os.ReadFile(s.historyPath)
	if err == nil {
		var history []session
		if json.Unmarshal(b, &history) == nil {
			s.History = mergeHistory(history, s.History)
		}
	}
	s.trimHistory()
	if inSettings && s.saveHistory() == nil {
		s.saveSettings()
	}
	return s
}

func mergeHistory(history, others []session) []session {
	if len(others) == 0 {
		// a continued party moved to the end; sorting by start would bury it again
		return history
	}
	for _, o := range others {
		if !slices.ContainsFunc(history, func(h session) bool { return h.Started.Equal(o.Started) && h.File == o.File }) {
			history = append(history, o)
		}
	}
	slices.SortStableFunc(history, func(a, b session) int { return a.Started.Compare(b.Started) })
	return history
}

func (s *store) trimHistory() {
	if over := len(s.History) - maxHistory; over > 0 {
		s.History = slices.Delete(s.History, 0, over)
	}
}

func (s *store) saveSettings() error {
	return writeJSON(s.path, struct {
		Settings settings `json:"settings"`
	}{s.Settings})
}

func (s *store) saveHistory() error {
	return writeJSON(s.historyPath, s.History)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(b)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *store) begin(f form) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Settings.Hostname = strings.TrimSpace(f.Hostname)
	if s.Settings.Relay == "" && f.Mode == "relay" {
		s.Settings.Relay = strings.TrimSpace(f.Relay)
	}
	if f.Mode == "direct" {
		s.Settings.Local = f.Local
		if p := strings.TrimSpace(f.Port); p != "" {
			s.Settings.Port = p
		}
	}
	if i := s.find(f.Continue); i >= 0 {
		// the same room and secret, so the same party: it moves to the top, open again
		sess := s.History[i]
		sess.Ended = time.Time{}
		s.History = append(slices.Delete(s.History, i, i+1), sess)
		s.saveSettings()
		s.saveHistory()
		return len(s.History) - 1
	}
	sess := session{Started: time.Now(), File: strings.TrimSpace(f.File), Playlist: f.Playlist, Mode: f.Mode, Secret: strings.TrimSpace(f.Secret), People: []string{}}
	if f.Mode == "relay" {
		sess.Relay, sess.Room = strings.TrimSpace(f.Relay), strings.TrimSpace(f.Room)
	} else {
		sess.Addrs = strings.TrimSpace(f.Addrs)
	}
	s.History = append(s.History, sess)
	s.trimHistory()
	s.saveSettings()
	s.saveHistory()
	return len(s.History) - 1
}

func (s *store) find(started string) int {
	t, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return -1
	}
	return slices.IndexFunc(s.History, func(h session) bool { return h.Started.Equal(t) })
}

// stoppedAt is where the party at i continues: the entry and the second.
func (s *store) stoppedAt(i int) (string, float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.History) {
		return "", 0
	}
	return s.History[i].Stopped.Entry, s.History[i].Stopped.At
}

func (s *store) observe(i int, line string) {
	person := ""
	if after, ok := strings.CutPrefix(message(line), "your address to share is "); ok {
		s.update(i, func(sess *session) {
			sess.Invite = strings.TrimSpace(after)
			fields := strings.Fields(sess.Invite)
			for j := 0; j+1 < len(fields); j++ {
				switch fields[j] {
				case "-room":
					sess.Room = fields[j+1]
				case "-secret":
					sess.Secret = fields[j+1]
				}
			}
		})
		return
	}
	if strings.HasPrefix(message(line), "finished: ") {
		s.update(i, func(sess *session) { sess.Finished = true })
		return
	}
	if after, ok := strings.CutPrefix(message(line), "stopped at: "); ok {
		secs, name, playlist := strings.Cut(after, " in ")
		var to stop
		var err error
		if to.At, err = strconv.ParseFloat(strings.TrimSpace(secs), 64); err != nil {
			return
		}
		if playlist && json.Unmarshal([]byte(name), &to.Entry) != nil {
			return
		}
		s.update(i, func(sess *session) { sess.Stopped = to })
		return
	}
	if after, ok := strings.CutPrefix(message(line), "now playing: "); ok {
		var name string
		if json.Unmarshal([]byte(after), &name) != nil {
			return
		}
		s.update(i, func(sess *session) { sess.openVideo(name, time.Now()) })
		return
	}
	if text, ok := notice(line); ok {
		if name, ok := strings.CutSuffix(text, mpv.HasJoined); ok {
			person = name
		} else if name, ok := strings.CutPrefix(text, mpv.ConnectedTo); ok {
			person = name
		}
	}
	if person == "" {
		return
	}
	s.update(i, func(sess *session) {
		if !slices.Contains(sess.People, person) {
			sess.People = append(sess.People, person)
		}
	})
}

func (s *store) end(i int) {
	s.update(i, func(sess *session) {
		sess.Ended = time.Now()
		sess.finishVideo(sess.Ended)
	})
}

func (s *store) update(i int, f func(*session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.History) {
		return
	}
	f(&s.History[i])
	s.saveHistory()
}

func message(line string) string {
	if len(line) < len("2006/01/02 15:04:05 ") {
		return ""
	}
	return line[len("2006/01/02 15:04:05 "):]
}

func notice(line string) (string, bool) {
	body, ok := strings.CutPrefix(message(line), "do: sent request: ")
	if !ok {
		return "", false
	}
	req := struct {
		Command []any `json:"command"`
	}{}
	if json.Unmarshal([]byte(body), &req) != nil || len(req.Command) < 2 || req.Command[0] != "show-text" {
		return "", false
	}
	text, ok := req.Command[1].(string)
	return text, ok
}
