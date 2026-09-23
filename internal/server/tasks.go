package server

import (
	"net/http"

	"github.com/jhgaylor/wisp/internal/store"
)

// Upstream's Tasks API (explicit keep-awake holds) is served inside the sprite,
// on /.sprite/api.sock, and the guest agent owns the table. These routes expose
// the same API from outside as /v1/sprites/{name}/tasks; that part is a
// wisp extension. The lifecycle engine honors holds in watch().

func (s *Server) registerTasks(mux *http.ServeMux) {
	mux.HandleFunc("/v1/sprites/{name}/tasks", s.tasks)
	mux.HandleFunc("/v1/sprites/{name}/tasks/{task}", s.tasks)
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		sp, ok := s.lookup(w, r)
		if !ok {
			return
		}
		// A task is a hold on the current run, so a sprite that is not running has
		// none. Answer that without waking it just to ask.
		if s.life.Status(sp) != "running" {
			if r.PathValue("task") != "" {
				writeErr(w, http.StatusNotFound, "not_found", "task not found")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"tasks": []any{}})
			return
		}
	}
	s.proxyAgent(w, r)
}

// noteHold logs when a sprite starts and stops being held awake by tasks, so
// "why is this VM still running" has an answer in the log.
func (l *Lifecycle) noteHold(sp store.Sprite, held *bool, tasks int) {
	if now := tasks > 0; now != *held {
		*held = now
		if now {
			l.log.Info("sprite held awake by tasks", "sprite", sp.Name, "tasks", tasks)
		} else {
			l.log.Info("sprite no longer held by tasks", "sprite", sp.Name)
		}
	}
}
