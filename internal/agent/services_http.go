package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (s *Server) registerServices(mux *http.ServeMux) {
	mux.HandleFunc("GET /services", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Services.List())
	})
	mux.HandleFunc("GET /services/{name}", func(w http.ResponseWriter, r *http.Request) {
		svc, err := s.Services.Get(r.PathValue("name"))
		if err != nil {
			serviceErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, svc)
	})
	mux.HandleFunc("PUT /services/{name}", s.putService)
	mux.HandleFunc("DELETE /services/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Services.Delete(r.PathValue("name")); err != nil {
			serviceErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /services/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		s.streamAction(w, r, true, func(name string) error { return s.Services.Start(name) })
	})
	mux.HandleFunc("POST /services/{name}/restart", func(w http.ResponseWriter, r *http.Request) {
		s.streamAction(w, r, true, func(name string) error { return s.Services.Restart(name, stopTimeout(r)) })
	})
	mux.HandleFunc("POST /services/{name}/stop", func(w http.ResponseWriter, r *http.Request) {
		s.streamAction(w, r, false, func(name string) error { return s.Services.Stop(name, stopTimeout(r)) })
	})
	mux.HandleFunc("POST /services/signal", s.signalService)
	mux.HandleFunc("GET /services/{name}/logs", s.serviceLogs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func serviceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrServiceNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrServiceConflict):
		writeErr(w, http.StatusConflict, "conflict", strings.TrimPrefix(err.Error(), ErrServiceConflict.Error()+": "))
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

func stopTimeout(r *http.Request) time.Duration {
	d, _ := parseDuration(r.URL.Query().Get("timeout"), defaultStopTimeout)
	return d
}

func (s *Server) putService(w http.ResponseWriter, r *http.Request) {
	var def ServiceDef
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&def); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	def.Name = r.PathValue("name")
	// Define first so the stream can subscribe before the service starts.
	if err := s.Services.Define(def); err != nil {
		serviceErr(w, err)
		return
	}
	s.streamAction(w, r, true, func(name string) error { return s.Services.Start(name) })
}

// streamAction runs a lifecycle action and streams the service's events as
// NDJSON. When watching, it keeps streaming logs for ?duration (default 5s) so
// the caller sees whether the service stays up; otherwise it ends with the action.
func (s *Server) streamAction(w http.ResponseWriter, r *http.Request, watch bool, action func(name string) error) {
	name := r.PathValue("name")
	watchFor, err := parseDuration(r.URL.Query().Get("duration"), 5*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid duration")
		return
	}
	events, cancel, err := s.Services.Subscribe(name)
	if err != nil {
		serviceErr(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(ev ServiceEvent) {
		if ev.Timestamp == 0 {
			ev.Timestamp = time.Now().UnixMilli()
		}
		enc.Encode(ev)
		if fl != nil {
			fl.Flush()
		}
	}

	acted := make(chan error, 1)
	go func() { acted <- action(name) }()
	var deadline <-chan time.Time
	if watch {
		deadline = time.After(watchFor)
	}
	for actionDone := false; !(actionDone && !watch); {
		select {
		case ev := <-events:
			send(ev)
			continue
		case err := <-acted:
			actionDone = true
			if err != nil {
				send(ServiceEvent{Type: "error", Data: err.Error()})
			}
			continue
		case <-deadline:
		case <-r.Context().Done():
			return
		}
		break
	}
	for drained := false; !drained; { // events emitted just before the action returned
		select {
		case ev := <-events:
			send(ev)
		default:
			drained = true
		}
	}
	send(ServiceEvent{Type: "complete", LogFiles: map[string]string{"combined": s.Services.LogPath(name)}})
}

func (s *Server) signalService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	sig, err := ParseSignal(req.Signal)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.Services.Signal(req.Name, sig); err != nil {
		serviceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serviceLogs returns the tail of a service's log file as NDJSON events.
func (s *Server) serviceLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.Services.Get(name); err != nil {
		serviceErr(w, err)
		return
	}
	lines := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && n > 0 {
		lines = n
	}
	var tail []string
	if f, err := os.Open(s.Services.LogPath(name)); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if tail = append(tail, sc.Text()); len(tail) > lines {
				tail = tail[1:]
			}
		}
		f.Close()
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, l := range tail {
		// "<rfc3339> [stream] text"
		ev := ServiceEvent{Type: "stdout", Data: l}
		if ts, rest, ok := strings.Cut(l, " ["); ok {
			if stream, text, ok := strings.Cut(rest, "] "); ok {
				ev.Type, ev.Data = stream, text
				if t, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err == nil {
					ev.Timestamp = t.UnixMilli()
				}
			}
		}
		enc.Encode(ev)
	}
	enc.Encode(ServiceEvent{Type: "complete", Timestamp: time.Now().UnixMilli(),
		LogFiles: map[string]string{"combined": s.Services.LogPath(name)}})
}
