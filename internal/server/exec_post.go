package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
)

// execPost serves upstream's HTTP exec: a body of type-prefixed frames with no
// length field, where every frame must be exactly one HTTP chunk. A generic
// reverse proxy coalesces and splits chunks at will, so this hop is done by
// hand: the agent sends length-framed data over vsock, and each frame is
// written and flushed to the client as its own chunk.
func (s *Server) execPost(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	m, release, err := s.life.Acquire(r.Context(), sp)
	if err != nil {
		s.writeWakeErr(w, sp.Name, err)
		return
	}
	defer release()

	q := withSpriteEnv(r.URL.Query(), sp)
	q.Set("framing", "length")
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://agent/exec?"+q.Encode(), r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.ContentLength = r.ContentLength
	tr := &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.Dial(ctx) }}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "agent_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The command never started; pass the agent's JSON error through as is.
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	rc := http.NewResponseController(w)
	br := bufio.NewReader(resp.Body)
	head := make([]byte, 5)
	for {
		if _, err := io.ReadFull(br, head); err != nil {
			return // EOF after the exit frame, or the VM went away mid-command
		}
		frame := make([]byte, 1+binary.BigEndian.Uint32(head[1:]))
		frame[0] = head[0]
		if _, err := io.ReadFull(br, frame[1:]); err != nil {
			return
		}
		// A write larger than net/http's 4 KiB buffer bypasses it, and a smaller
		// one is pushed out by the flush: either way, one frame is one chunk.
		if _, err := w.Write(frame); err != nil {
			return
		}
		rc.Flush()
	}
}
