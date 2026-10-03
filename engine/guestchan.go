package engine

import (
	"net/http"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The guest channel is how a sprite asks the host for things only the host can
// do (checkpoints). It rides Firecracker's guest-initiated vsock: the guest
// connects to CID 2 port guestAPIPort, which Firecracker turns into a
// connection to the unix socket v.sock_<port> in the machine directory.
//
// The channel is the identity. Each VM gets its own listener whose handler is
// bound to that one sprite, so there is no token and no sprite name in the
// paths: a guest cannot address anything but itself.

// guestAPIPort must match hostAPIPort in cmd/wisp-agent.
const guestAPIPort = 1025

// GuestChan is one VM's channel. It lives exactly as long as the VM process:
// opened before boot or snapshot restore, closed by cleanupLocked.
type GuestChan struct {
	srv *http.Server
}

// SetGuestAPI installs the handler a VM's guest channel serves: f builds one
// per VM, bound to that VM's record and channel. Until then a guest gets 404.
// Install it before anything boots.
func (l *Engine) SetGuestAPI(f func(store.Record, *GuestChan) http.Handler) { l.guestAPI = f }

func (l *Engine) openGuestChan(sp store.Record) (*GuestChan, error) {
	ln, err := vmm.ListenGuest(l.store.Dir(sp.ID), guestAPIPort)
	if err != nil {
		return nil, err
	}
	g := &GuestChan{}
	var h http.Handler = http.NotFoundHandler()
	if l.guestAPI != nil {
		h = l.guestAPI(sp, g)
	}
	g.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go g.srv.Serve(ln)
	return g, nil
}

// close is safe on nil. Closing the listener also unlinks its socket file.
func (g *GuestChan) close() {
	if g != nil {
		g.srv.Close()
	}
}
