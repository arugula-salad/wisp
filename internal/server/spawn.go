package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
)

// Spawning is a sprite creating and managing sprites of its own, which is what
// an app that hands every visitor a fresh sprite needs. Guests are firewalled
// off the host, and a bearer token inside one would be the whole API, so this
// rides the guest channel instead: the channel already says which sprite is
// asking, and it serves only what is listed here.
//
// What a spawner gets is deliberately narrower than the public API:
//   - it sees, and can delete, only the sprites it created;
//   - it can hold at most spawn_policy.max_children of them;
//   - a child runs under the spawner's network policy, so spawning is not a way
//     out of one, and gets the spawner's machine shape unless it is a clone;
//   - it can clone only its own checkpoints, its children's, and those of the
//     sprites the operator listed in spawn_policy.sources;
//   - the policy is set from outside only, and a child never starts with one.

const defaultMaxChildren = 10

// cloneFrom names the checkpoint a new sprite starts from.
type cloneFrom struct {
	// Sprite may be omitted from inside a sprite, where it means the caller.
	Sprite string `json:"sprite"`
	// Checkpoint defaults to the source's newest manual checkpoint.
	Checkpoint string `json:"checkpoint"`
	// Image starts the sprite from a container image instead (images.go). From
	// inside a sprite it must already be in the host's image cache.
	Image string `json:"image,omitempty"`
}

type createError struct {
	status    int
	code, msg string
}

// cloneSource resolves from to a sprite and one of its checkpoints. The source
// stays locked until unlock, which keeps the checkpoint from being deleted
// while it is read.
func (s *Server) cloneSource(from cloneFrom, parent *store.Sprite) (src store.Sprite, checkpoint string, unlock func(), _ *createError) {
	notFound := &createError{http.StatusNotFound, "source_not_found", "from.sprite: no such sprite"}
	name := from.Sprite
	if name == "" {
		if parent == nil {
			return src, "", nil, &createError{http.StatusBadRequest, "bad_request", "from.sprite is required"}
		}
		name = parent.Name
	}
	src, err := s.store.Get(name)
	if err != nil {
		return src, "", nil, notFound
	}
	if parent != nil && src.ID != parent.ID && src.ParentID != parent.ID && !slices.Contains(spawnPolicy(*parent).Sources, src.Name) {
		// The same answer as for a sprite that does not exist: a spawner learns
		// nothing about sprites that are not its business.
		return src, "", nil, notFound
	}
	rt := s.life.rt(src.ID)
	rt.mu.Lock()
	if src, err = s.store.Get(name); err != nil { // re-read under the lock
		rt.mu.Unlock()
		return src, "", nil, notFound
	}
	checkpoint = from.Checkpoint
	if checkpoint == "" {
		if cps := filterCheckpoints(src, "", false); len(cps) > 0 {
			checkpoint = cps[0].ID
		}
	}
	if checkpoint == "" || findCheckpoint(src, checkpoint) == nil {
		rt.mu.Unlock()
		return src, "", nil, &createError{http.StatusNotFound, "checkpoint_not_found",
			fmt.Sprintf("sprite %q has no such checkpoint to clone; create one first", src.Name)}
	}
	return src, checkpoint, rt.mu.Unlock, nil
}

func spawnPolicy(sp store.Sprite) store.SpawnPolicy {
	if sp.Spawn == nil {
		return store.SpawnPolicy{}
	}
	p := *sp.Spawn
	if p.MaxChildren <= 0 {
		p.MaxChildren = defaultMaxChildren
	}
	return p
}

func (s *Server) children(parent store.Sprite) []store.Sprite {
	var out []store.Sprite
	for _, sp := range s.store.List("") {
		if sp.ParentID == parent.ID {
			out = append(out, sp)
		}
	}
	return out
}

// childLimit is approximate under concurrent creates, which is fine for a ceiling.
func (s *Server) childLimit(parent store.Sprite) *LimitError {
	limit, n := spawnPolicy(parent).MaxChildren, len(s.children(parent))
	if n < limit {
		return nil
	}
	return &LimitError{Code: codeSpriteLimit, Limit: limit, Current: n,
		Message: fmt.Sprintf("this sprite already holds %d sprites, the most it may create (spawn policy max_children); delete one first", n)}
}

// inherit makes sp a child of parent. The network policy is always the
// parent's, whatever a cloned source had; the machine shape is the parent's
// unless the clone brought its own. Nothing here is the child's to choose.
func inherit(sp *store.Sprite, parent store.Sprite, cloned bool) {
	sp.ParentID = parent.ID
	sp.NetworkRules = parent.NetworkRules
	if !cloned {
		sp.Config, sp.Privileges, sp.Resources = parent.Config, parent.Privileges, parent.Resources
	}
}

// registerGuestSpawn adds the sprite routes to one sprite's guest channel.
func (s *Server) registerGuestSpawn(mux *http.ServeMux, bind func(guestHandler) http.HandlerFunc) {
	// spawner refuses a sprite without the policy; the policy is read per
	// request, so granting and revoking it need no reboot.
	spawner := func(h func(http.ResponseWriter, *http.Request, store.Sprite)) http.HandlerFunc {
		return bind(func(w http.ResponseWriter, r *http.Request, self store.Sprite, _ *guestChan) {
			if !spawnPolicy(self).Enabled {
				s.spawnRefused(w, self)
				return
			}
			h(w, r, self)
		})
	}
	child := func(h func(http.ResponseWriter, store.Sprite)) http.HandlerFunc {
		return spawner(func(w http.ResponseWriter, r *http.Request, self store.Sprite) {
			sp, err := s.store.Get(r.PathValue("name"))
			if err != nil || sp.ParentID != self.ID {
				writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
				return
			}
			h(w, sp)
		})
	}
	mux.HandleFunc("POST /v1/sprites", spawner(func(w http.ResponseWriter, r *http.Request, self store.Sprite) {
		s.create(w, r, &self)
	}))
	mux.HandleFunc("GET /v1/sprites", spawner(func(w http.ResponseWriter, r *http.Request, self store.Sprite) {
		s.list(w, r, func(sp store.Sprite) bool { return sp.ParentID == self.ID })
	}))
	mux.HandleFunc("GET /v1/sprites/{name}", child(func(w http.ResponseWriter, sp store.Sprite) {
		writeJSON(w, http.StatusOK, s.render(sp))
	}))
	mux.HandleFunc("DELETE /v1/sprites/{name}", child(s.remove))
}

// spawnRefused answers a sprite without a spawn policy that asked for
// something only a spawner may do.
func (s *Server) spawnRefused(w http.ResponseWriter, self store.Sprite) {
	s.life.emit(self, "policy.denied", map[string]any{"policy": "spawn"})
	writeErr(w, http.StatusForbidden, "spawn_disabled",
		"this sprite may not manage sprites; enable it from outside with POST /v1/sprites/"+self.Name+"/policy/spawn")
}

func (s *Server) registerSpawnPolicy(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sprites/{name}/policy/spawn", func(w http.ResponseWriter, r *http.Request) {
		if sp, ok := s.lookup(w, r); ok {
			writeJSON(w, http.StatusOK, orZero(sp.Spawn))
		}
	})
	mux.HandleFunc("POST /v1/sprites/{name}/policy/spawn", func(w http.ResponseWriter, r *http.Request) {
		var p store.SpawnPolicy
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		if p.MaxChildren < 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "max_children must not be negative")
			return
		}
		for _, name := range p.Sources {
			if !nameRE.MatchString(name) {
				writeErr(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("sources: %q is not a sprite name", name))
				return
			}
		}
		s.setSpawnPolicy(w, r, &p)
	})
	mux.HandleFunc("DELETE /v1/sprites/{name}/policy/spawn", func(w http.ResponseWriter, r *http.Request) {
		s.setSpawnPolicy(w, r, nil)
	})
}

// setSpawnPolicy is not storePolicy: nothing in the guest enforces this one, so
// there is nothing to push to a running sprite.
func (s *Server) setSpawnPolicy(w http.ResponseWriter, r *http.Request, p *store.SpawnPolicy) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if _, err := s.store.Update(sp.Name, func(sp *store.Sprite) {
		sp.Spawn = p
		sp.UpdatedAt = time.Now().UTC()
	}); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
		return
	}
	s.log.Info("spawn policy set", "sprite", sp.Name, "enabled", p != nil && p.Enabled)
	s.life.emit(sp, "policy.changed", map[string]any{"policy": "spawn", "enabled": p != nil && p.Enabled})
	w.WriteHeader(http.StatusNoContent)
}
