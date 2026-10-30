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

const maxHistory = 100

type settings struct {
	Hostname  string `json:"hostname"`
	Relay     string `json:"relay"`
	Mpv       string `json:"mpv"`
	MpvFlags  string `json:"mpvFlags"`
	Cooldown  string `json:"cooldown"`
	CreateKey string `json:"createKey"`
	PPort     string `json:"pport"`
	Local     bool   `json:"local"`
	Port      string `json:"port"`
}

type session struct {
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	File    string    `json:"file"`
	Mode    string    `json:"mode"`
	Relay   string    `json:"relay,omitempty"`
	Room    string    `json:"room,omitempty"`
	Secret  string    `json:"secret,omitempty"`
	Addrs   string    `json:"addrs,omitempty"`
	Invite  string    `json:"invite,omitempty"`
	People  []string  `json:"people"`
}

type store struct {
	path string

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
	s := &store{path: path, Settings: settings{Local: true}}
	b, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal(b, s)
	}
	return s
}

// save must be called with s.mu held.
func (s *store) save() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	err = os.MkdirAll(filepath.Dir(s.path), 0o700)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".gui-*.json")
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
	return os.Rename(tmp.Name(), s.path)
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
	sess := session{Started: time.Now(), File: strings.TrimSpace(f.File), Mode: f.Mode, People: []string{}}
	if f.Mode == "relay" {
		sess.Relay, sess.Room, sess.Secret = strings.TrimSpace(f.Relay), strings.TrimSpace(f.Room), strings.TrimSpace(f.Secret)
	} else {
		sess.Addrs = strings.TrimSpace(f.Addrs)
	}
	s.History = append(s.History, sess)
	if over := len(s.History) - maxHistory; over > 0 {
		s.History = slices.Delete(s.History, 0, over)
	}
	s.save()
	return len(s.History) - 1
}

// observe records what a party's log line says about the session at index i.
func (s *store) observe(i int, line string) {
	person := ""
	if _, after, ok := strings.Cut(line, "your address to share is "); ok {
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
	s.update(i, func(sess *session) { sess.Ended = time.Now() })
}

func (s *store) update(i int, f func(*session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.History) {
		return
	}
	f(&s.History[i])
	s.save()
}

// notice extracts the text of an on-screen notice from mpv's "do: sent request:" log line.
func notice(line string) (string, bool) {
	_, body, ok := strings.Cut(line, "do: sent request: ")
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
