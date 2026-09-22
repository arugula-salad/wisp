package server

import (
	"net/http"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

// The guest channel is how a sprite asks the host for things only the host can
// do (checkpoints). It rides Firecracker's guest-initiated vsock: the guest
// connects to CID 2 port guestAPIPort, which Firecracker turns into a
// connection to the unix socket v.sock_<port> in the machine directory.
//
// The channel is the identity. Each VM gets its own listener whose handler is
// bound to that one sprite, so there is no token and no sprite name in the
// paths: a guest cannot address anything but itself.

// guestAPIPort must match hostAPIPort in cmd/sprite-agent.
const guestAPIPort = 1025

// guestChan is one VM's channel. It lives exactly as long as the VM process:
// opened before boot or snapshot restore, closed by cleanupLocked.
type guestChan struct {
	srv *http.Server
}

func (l *Lifecycle) openGuestChan(sp store.Sprite) (*guestChan, error) {
	ln, err := vmm.ListenGuest(l.store.Dir(sp.ID), guestAPIPort)
	if err != nil {
		return nil, err
	}
	g := &guestChan{}
	var h http.Handler = http.NotFoundHandler()
	if l.guestAPI != nil {
		h = l.guestAPI(sp, g)
	}
	g.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go g.srv.Serve(ln)
	return g, nil
}

// close is safe on nil. Closing the listener also unlinks its socket file.
func (g *guestChan) close() {
	if g != nil {
		g.srv.Close()
	}
}

// guestHandler serves a request from inside the sprite it is given.
type guestHandler func(http.ResponseWriter, *http.Request, store.Sprite, *guestChan)

// guestAPI is the handler behind one sprite's channel. It mirrors the public
// checkpoint routes with the /sprites/{name} part removed, the way upstream's
// /.sprite/api.sock does. The /v1/sprites routes are for a sprite that may
// create sprites of its own (spawn.go).
func (s *Server) guestAPI(sp store.Sprite, g *guestChan) http.Handler {
	bind := func(h guestHandler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// Names are reusable after a delete; the ID pins this channel to the sprite it was opened for.
			cur, err := s.store.Get(sp.Name)
			if err != nil || cur.ID != sp.ID {
				writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
				return
			}
			// A request from inside is activity like one from outside. It also has to
			// be: suspending between the handler's last write and the guest reading
			// it would turn every answer into a reset connection on resume.
			rt := s.life.rt(sp.ID)
			rt.begin()
			defer rt.end()
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
	return s.instrument(func(*http.Request) string { return kindGuest }, false, sp.Name, mux)
}
