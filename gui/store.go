package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
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
	People   []string  `json:"people"`
	Videos   []played  `json:"videos,omitempty"`
}

type played struct {
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
}

func (sess *session) startVideo(name string, at time.Time) {
	if n := len(sess.Videos); n > 0 && sess.Videos[n-1].Name == name {
		sess.Videos[n-1].Ended = time.Time{}
		return
	}
	sess.Videos = append(sess.Videos, played{Name: name, Started: at})
	if over := len(sess.Videos) - maxVideos; over > 0 {
		sess.Videos = slices.Delete(sess.Videos, 0, over)
	}
}

func (sess *session) finishVideo(at time.Time) {
	if n := len(sess.Videos); n > 0 && sess.Videos[n-1].Ended.IsZero() {
		sess.Videos[n-1].Ended = at
	}
}

type store struct {
	path        string
	historyPath string
	onFile      string

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
	s.onFile = ""
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
	if after, ok := strings.CutPrefix(message(line), "now playing: "); ok {
		var name string
		if json.Unmarshal([]byte(after), &name) != nil {
			return
		}
		s.update(i, func(sess *session) {
			s.onFile = name
			sess.finishVideo(time.Now())
		})
		return
	}
	if message(line) == "playback started" {
		s.update(i, func(sess *session) {
			if s.onFile != "" {
				sess.startVideo(s.onFile, time.Now())
			}
		})
		return
	}
	if text, ok := notice(line); ok {
		if name, ok := strings.CutSuffix(text, " has joined"); ok {
			person = name
		} else if name, ok := strings.CutPrefix(text, "connected to "); ok {
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
