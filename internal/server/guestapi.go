package server

// The Sprites API a guest reaches over its host channel (engine/guestchan.go):
// the handler the Server installs on every VM's channel.

import (
	"net/http"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/httpstats"
	"github.com/arugula-salad/wisp/internal/store"
)

// guestHandler serves a request from inside the sprite it is given.
type guestHandler func(http.ResponseWriter, *http.Request, store.Sprite, *engine.GuestChan)

// guestAPI is the handler behind one sprite's channel. It mirrors the public
// checkpoint routes with the /sprites/{name} part removed, the way upstream's
// /.sprite/api.sock does. The /v1/sprites routes are for a sprite that may
// create sprites of its own (spawn.go).
func (s *Server) guestAPI(rec store.Record, g *engine.GuestChan) http.Handler {
	// For its name: the VM is booting, so the record is there.
	sp, _ := s.store.Get(rec.ID)
	bind := func(h guestHandler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// By ID: names are reusable after a delete, IDs are not, so this is
			// the sprite the channel was opened for or nothing.
			cur, err := s.store.Get(sp.ID)
			if err != nil {
				writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
				return
			}
			// A request from inside is activity like one from outside. It also has to
			// be: suspending between the handler's last write and the guest reading
			// it would turn every answer into a reset connection on resume.
			defer s.life.BeginUse(sp.ID)()
			h(w, r, cur, g)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/checkpoint", bind(s.createCheckpoint))
	mux.HandleFunc("GET /v1/checkpoints", bind(s.listCheckpoints))
	mux.HandleFunc("GET /v1/checkpoints/{id}", bind(s.getCheckpoint))
	mux.HandleFunc("DELETE /v1/checkpoints/{id}", bind(s.deleteCheckpoint))
	mux.HandleFunc("POST /v1/checkpoints/{id}/restore", bind(s.restoreCheckpoint))
	mux.HandleFunc("POST /v1/checkpoints/{id}/mount", bind(s.mountCheckpoint))
	mux.HandleFunc("POST /v1/checkpoints/{id}/unmount", bind(s.unmountCheckpoint))
	s.registerGuestSpawn(mux, bind)
	s.registerGuestEvents(mux, sp)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.httpStats.Instrument(func(*http.Request) string { return httpstats.KindGuest }, false, sp.Name, mux)
}
