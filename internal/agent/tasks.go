package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The Tasks API is upstream's explicit keep-awake: while at least one task is
// live the sprite is not idle-suspended. Upstream serves it inside the sprite
// on /.sprite/api.sock, so the table lives here in the guest; spritesd learns
// about holds through /internal/activity. Every task expires (an hour at
// most), so a client that crashed cannot pin a VM forever.

const (
	maxTaskExpire = time.Hour
	maxTasks      = 64
	maxTaskName   = 128
)

type Task struct {
	Name      string    `json:"name"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// taskTable's zero value is ready to use.
type taskTable struct {
	mu sync.Mutex
	m  map[string]Task
}

// live drops expired tasks and returns the rest sorted by name.
func (t *taskTable) live() []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []Task{}
	for name, task := range t.m {
		if !time.Now().Before(task.ExpiresAt) {
			delete(t.m, name)
			continue
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (t *taskTable) get(name string) (Task, bool) {
	for _, task := range t.live() {
		if task.Name == name {
			return task, true
		}
	}
	return Task{}, false
}

var (
	errTaskExists = fmt.Errorf("task already exists")
	errTaskLimit  = fmt.Errorf("too many tasks (limit %d)", maxTasks)
)

// put creates a task or, unless createOnly, refreshes an existing one's expiry.
func (t *taskTable) put(name string, expire time.Duration, createOnly bool) (Task, error) {
	live := t.live()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	task, exists := t.m[name]
	switch {
	case exists && createOnly:
		return task, errTaskExists
	case !exists && len(live) >= maxTasks:
		return task, errTaskLimit
	case !exists:
		task = Task{Name: name, StartedAt: now}
	}
	task.ExpiresAt = now.Add(expire)
	if t.m == nil {
		t.m = map[string]Task{}
	}
	t.m[name] = task
	return task, nil
}

func (t *taskTable) delete(name string) bool {
	_, ok := t.get(name)
	t.mu.Lock()
	delete(t.m, name)
	t.mu.Unlock()
	return ok
}

// clear runs after a snapshot restore: a task is a hold on the current run,
// and a run that was suspended anyway (spritesd shutting down) is over.
func (t *taskTable) clear() {
	t.mu.Lock()
	t.m = nil
	t.mu.Unlock()
}

// registerTasks mounts the API under prefix: /v1/tasks on the guest socket,
// /tasks for spritesd, which exposes it as /v1/sprites/{name}/tasks.
func (s *Server) registerTasks(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"tasks": s.tasks.live()})
	})
	mux.HandleFunc("POST "+prefix, s.taskPut)
	mux.HandleFunc("PUT "+prefix, s.taskPut)
	mux.HandleFunc("PUT "+prefix+"/{task}", s.taskPut)
	mux.HandleFunc("GET "+prefix+"/{task}", func(w http.ResponseWriter, r *http.Request) {
		if task, ok := s.tasks.get(r.PathValue("task")); ok {
			writeJSON(w, http.StatusOK, task)
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "task not found")
	})
	mux.HandleFunc("DELETE "+prefix+"/{task}", func(w http.ResponseWriter, r *http.Request) {
		if !s.tasks.delete(r.PathValue("task")) {
			writeErr(w, http.StatusNotFound, "not_found", "task not found")
			return
		}
		// The idle window starts when the hold ends, not when it began.
		s.Sessions.touch()
		w.WriteHeader(http.StatusNoContent)
	})
}

// taskExpire parses "expire": integer seconds or a duration string. Absent means the maximum.
func taskExpire(raw json.RawMessage) (time.Duration, error) {
	d := maxTaskExpire
	if len(raw) > 0 && string(raw) != "null" {
		var n int64
		var str string
		var err error
		switch {
		case json.Unmarshal(raw, &n) == nil:
			d = time.Duration(n) * time.Second
		case json.Unmarshal(raw, &str) == nil:
			if d, err = parseDuration(str, 0); err != nil {
				return 0, fmt.Errorf("invalid expire %q", str)
			}
		default:
			return 0, fmt.Errorf("expire must be seconds or a duration string")
		}
	}
	if d <= 0 || d > maxTaskExpire {
		return 0, fmt.Errorf("expire must be positive and at most %s; refresh the task to hold longer", maxTaskExpire)
	}
	return d, nil
}

// taskPut is create (POST: 201, or 409 if the name is taken) and upsert (PUT: 200).
func (s *Server) taskPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string          `json:"name"`
		Expire json.RawMessage `json:"expire"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if name := r.PathValue("task"); name != "" {
		req.Name = name
	}
	if req.Name == "" || len(req.Name) > maxTaskName {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required (at most "+strconv.Itoa(maxTaskName)+" bytes)")
		return
	}
	expire, err := taskExpire(req.Expire)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	task, err := s.tasks.put(req.Name, expire, r.Method == http.MethodPost)
	switch {
	case err == errTaskExists:
		writeErr(w, http.StatusConflict, "conflict", "a task with that name already exists")
	case err != nil:
		writeErr(w, http.StatusTooManyRequests, "too_many_tasks", err.Error())
	case r.Method == http.MethodPost:
		writeJSON(w, http.StatusCreated, task)
	default:
		writeJSON(w, http.StatusOK, task)
	}
}
