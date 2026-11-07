package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/mhashemm/watchparty/update"
)

var version = "dev"

func (g *gui) checkUpdate(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"available"`
		Installed string `json:"installed,omitempty"`
		Error     string `json:"error,omitempty"`
	}{Current: version}
	g.mu.Lock()
	res.Installed = g.installed
	g.mu.Unlock()
	if version != "dev" {
		latest, err := update.Latest(r.Context())
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Latest = latest
			res.Available = update.Newer(latest, version)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func (g *gui) update(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	running := g.cancel != nil
	g.mu.Unlock()
	if running {
		http.Error(w, "end the party before updating", http.StatusConflict)
		return
	}
	if version == "dev" {
		http.Error(w, "development builds do not update themselves", http.StatusBadRequest)
		return
	}
	tag, err := update.Install(r.Context(), g.exe, version, "watchparty-gui")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	recordVersion(g.exe, tag)
	g.mu.Lock()
	g.installed = tag
	g.mu.Unlock()
	io.WriteString(w, tag)
}

func (g *gui) restart(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	running, installed := g.cancel != nil, g.installed
	g.mu.Unlock()
	if running {
		http.Error(w, "end the party before restarting", http.StatusConflict)
		return
	}
	if installed == "" {
		http.Error(w, "no update is waiting for a restart", http.StatusBadRequest)
		return
	}
	err := exec.Command(g.exe).Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, installed)
	go func() {
		time.Sleep(500 * time.Millisecond)
		g.closeWindow()
		g.quit()
	}()
}
