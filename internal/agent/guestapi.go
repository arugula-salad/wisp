package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
)

// The guest API is what processes inside the sprite (sprite-env, agents, curl)
// reach on /.sprite/api.sock, with no token: being in the sprite is the
// credential. It is deliberately much smaller than the vsock API spritesd
// uses. Services are handled here; checkpoints, and the sprites this one may
// create, are host operations, so they are relayed to spritesd over a channel
// that can only ever mean "this sprite is asking".
// Exec, the filesystem API, the TCP proxy and /internal/* stay off it: they run
// things as root or on spritesd's behalf, and nothing inside needs them.

// GuestAPI returns the handler for the in-guest socket. hostDial opens a
// stream to spritesd's per-sprite channel; nil leaves checkpoints unavailable.
func (s *Server) GuestAPI(hostDial func(ctx context.Context) (net.Conn, error)) http.Handler {
	mux := http.NewServeMux()
	if s.Services != nil {
		services := http.NewServeMux()
		s.registerServices(services)
		mux.Handle("/v1/services", http.StripPrefix("/v1", services))
		mux.Handle("/v1/services/", http.StripPrefix("/v1", services))
	}
	// Tasks are keep-awake holds. Upstream serves them only here, inside the guest.
	s.registerTasks(mux, "/v1/tasks")
	if hostDial != nil {
		host := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) { pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "host" },
			Transport: &http.Transport{DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return hostDial(ctx) }},
			FlushInterval: -1, // checkpoint progress is streamed
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				writeErr(w, http.StatusBadGateway, "host_unreachable", err.Error())
			},
		}
		// More specific than the proxied prefix below, so these two are ours: mounting
		// needs work on both sides of the channel.
		s.registerCheckpointMounts(mux, hostDial)
		// Only these paths leave the guest; spritesd's side serves nothing else either.
		mux.Handle("/v1/checkpoint", host)
		mux.Handle("/v1/checkpoints", host)
		mux.Handle("/v1/checkpoints/", host)
		// Sprites this one created. spritesd answers 403 unless its spawn policy allows it.
		mux.Handle("/v1/sprites", host)
		mux.Handle("/v1/sprites/", host)
		// Events about those sprites (ours; spritesd scopes it to them).
		mux.Handle("/mini-sprites/v1/events", host)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return mux
}

// ListenGuestAPI creates the socket, replacing one left on disk by a previous
// boot. It belongs to the account commands run as, so that user needs no sudo
// while other local users get nothing: defining a service means running
// commands as that account.
func ListenGuestAPI(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if cred, _, _ := defaultUser(); cred != nil {
		os.Chown(path, int(cred.Uid), int(cred.Gid))
	}
	os.Chmod(path, 0o660)
	return ln, nil
}
