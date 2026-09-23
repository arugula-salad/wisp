// Package netd is the protocol between wispd (unprivileged) and
// wisp-netd (root). The helper exists because editing an nftables set
// needs CAP_NET_ADMIN, and it is deliberately incapable of anything else: one
// request type, which replaces the members of one set with addresses from one network.
package netd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	DefaultSocket = "/run/wisp/netd.sock"
	// The set setup-host.sh declares; its rules divert members to wispd.
	nftSet = "inet wisp restricted4"
	// A /16 cannot hold more sprites than this, so neither can a valid request.
	maxAddrs   = 1 << 16
	maxRequest = 2 << 20
)

// Request is the whole protocol: one JSON object per connection.
type Request struct {
	// Restricted4 is the complete new membership, not a delta, so a lost
	// message can never leave the two sides disagreeing for long.
	Restricted4 []string `json:"restricted4"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Push replaces the restricted set's membership via the helper at socket.
func Push(ctx context.Context, socket string, addrs []netip.Addr) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	req := Request{Restricted4: make([]string, 0, len(addrs))}
	for _, a := range addrs {
		req.Restricted4 = append(req.Restricted4, a.String())
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return err
	}
	var resp Response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return fmt.Errorf("no reply from helper: %w", err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// Server is the helper's side.
type Server struct {
	// Net is the sprite network. Nothing outside it can be put in the set, so
	// the worst a compromised wispd can do is restrict its own sprites.
	Net netip.Prefix
	// OwnerUID is the only non-root user whose requests are honoured. The
	// socket's mode already says so; this does not depend on the mode surviving.
	OwnerUID int
	// Apply runs an nft script. Injected so tests need neither root nor nft.
	Apply func(script string) error
	Log   *slog.Logger
}

func (s *Server) Serve(ln *net.UnixListener) error {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		// One at a time: requests are full replacements, so order is meaning.
		s.handle(c)
	}
}

func (s *Server) handle(c *net.UnixConn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	resp := Response{OK: true}
	n, err := s.serve(c)
	if err != nil {
		resp = Response{Error: err.Error()}
		s.Log.Warn("request refused", "err", err)
	} else {
		s.Log.Info("restricted set replaced", "members", n)
	}
	json.NewEncoder(c).Encode(resp)
}

func (s *Server) serve(c *net.UnixConn) (int, error) {
	// The request is read before anything is refused: replying and closing while the
	// client is still writing would cost it our explanation (it sees EPIPE instead).
	dec := json.NewDecoder(io.LimitReader(c, maxRequest))
	dec.DisallowUnknownFields()
	var req Request
	decodeErr := dec.Decode(&req)
	if uid, err := peerUID(c); err != nil {
		return 0, fmt.Errorf("cannot identify peer: %w", err)
	} else if uid != 0 && uid != s.OwnerUID {
		return 0, fmt.Errorf("uid %d is not allowed", uid)
	}
	if decodeErr != nil {
		return 0, fmt.Errorf("bad request: %w", decodeErr)
	}
	script, err := Script(s.Net, req.Restricted4)
	if err != nil {
		return 0, err
	}
	if err := s.Apply(script); err != nil {
		return 0, fmt.Errorf("nft: %w", err)
	}
	return len(req.Restricted4), nil
}

// Script validates the requested membership and renders the nft commands for it.
// nft applies one -f input as a single transaction, so flush + add swaps the
// membership atomically: there is no instant at which a restricted sprite is absent.
func Script(network netip.Prefix, members []string) (string, error) {
	if len(members) > maxAddrs {
		return "", fmt.Errorf("too many addresses (%d)", len(members))
	}
	gateway := network.Masked().Addr().Next()
	elems := make([]string, 0, len(members))
	seen := map[netip.Addr]bool{}
	for _, m := range members {
		a, err := netip.ParseAddr(m)
		if err != nil {
			return "", fmt.Errorf("invalid address %q", m)
		}
		// Only what ParseAddr understood is ever rendered, never the client's string.
		if !a.Is4() || !network.Contains(a) {
			return "", fmt.Errorf("address %s is outside the sprite network %s", a, network)
		}
		if a == gateway || a == network.Masked().Addr() {
			return "", fmt.Errorf("address %s is not a sprite address", a)
		}
		if !seen[a] {
			seen[a] = true
			elems = append(elems, a.String())
		}
	}
	script := "flush set " + nftSet + "\n"
	if len(elems) > 0 {
		script += "add element " + nftSet + " { " + strings.Join(elems, ", ") + " }\n"
	}
	return script, nil
}

// NftApply feeds scripts to the nft binary on stdin.
func NftApply(nft string) func(string) error {
	return func(script string) error {
		cmd := exec.Command(nft, "-f", "-")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Uid), nil
}
