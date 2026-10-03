package modal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/frontend/modal/modalpb"
)

// state is what the front end keeps outside the engine's records: apps (Modal
// metadata with no VM of their own), the images it has handed out, and every
// sandbox it created with how it ended, so that a finished sandbox, whose
// record is gone, still answers Sandbox.from_id and wait. It is one small
// JSON file, rewritten whole on every change.
type state struct {
	path string
	mu   sync.Mutex
	d    stateData
}

type stateData struct {
	// Apps maps environment name, then app name, to app ID.
	Apps map[string]map[string]string `json:"apps"`
	// Images maps an image ID to the guest disk it stands for (an images key).
	Images map[string]string `json:"images"`
	// Sandboxes is every sandbox made, running or finished.
	Sandboxes map[string]*sandboxState `json:"sandboxes"`
}

type sandboxState struct {
	AppID    string    `json:"appID"`
	Deadline time.Time `json:"deadline"`
	// Result is set when it has ended: by its entrypoint exiting, a
	// terminate, or its timeout.
	Result *result `json:"result,omitempty"`
	// EndedAt is when, for forgetting it in time.
	EndedAt time.Time `json:"endedAt,omitzero"`
}

// result is a GenericResult, the part the client reads.
type result struct {
	Status    modalpb.GenericResult_GenericStatus `json:"status"`
	ExitCode  int32                               `json:"exitCode,omitempty"`
	Exception string                              `json:"exception,omitempty"`
}

func (r *result) proto() *modalpb.GenericResult {
	if r == nil {
		return &modalpb.GenericResult{}
	}
	return &modalpb.GenericResult{Status: r.Status, Exitcode: r.ExitCode, Exception: r.Exception}
}

// keepEnded is how long a finished sandbox's result is kept.
const keepEnded = 7 * 24 * time.Hour

func openState(path string) (*state, error) {
	s := &state{path: path}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &s.d); err != nil {
			return nil, err
		}
	}
	if s.d.Apps == nil {
		s.d.Apps = map[string]map[string]string{}
	}
	if s.d.Images == nil {
		s.d.Images = map[string]string{}
	}
	if s.d.Sandboxes == nil {
		s.d.Sandboxes = map[string]*sandboxState{}
	}
	return s, nil
}

// update changes the state under its lock and saves it.
func (s *state) update(fn func(*stateData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.d); err != nil {
		return err
	}
	now := time.Now()
	for id, sb := range s.d.Sandboxes {
		if sb.Result != nil && now.Sub(sb.EndedAt) > keepEnded {
			delete(s.d.Sandboxes, id)
		}
	}
	return s.save()
}

func (s *state) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// view reads the state under its lock.
func (s *state) view(fn func(*stateData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}

// finish records how a sandbox ended, unless it already has: the first end
// (entrypoint exit, terminate, timeout) is the one that counts.
func (s *state) finish(id string, r result) (first bool) {
	s.update(func(d *stateData) error {
		sb := d.Sandboxes[id]
		if sb == nil || sb.Result != nil {
			return nil
		}
		sb.Result, sb.EndedAt, first = &r, time.Now(), true
		return nil
	})
	return first
}
