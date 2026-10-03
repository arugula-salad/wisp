package engine

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/arugula-salad/wisp/internal/vmm"
)

// bufferedConn replays bytes the HTTP response parser read past the 101.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// DialPort opens a raw TCP stream to localhost:port inside the guest,
// tunnelled over vsock through the guest agent, so it needs no guest network.
// port is a number, or "http" for the sprite's URL target (its HTTP service,
// else 8080). m is a VM Acquire returned, held for as long as the stream is
// used; a boot hook (OnBoot) may dial the VM it is given.
func DialPort(ctx context.Context, m *vmm.Machine, port string) (net.Conn, error) {
	conn, err := m.Dial(ctx)
	if err != nil {
		return nil, err
	}
	// The upgrade exchange below is plain blocking I/O, and the agent's side of
	// it can take its time (it starts the sprite's HTTP service on demand and
	// waits for it), so a caller's deadline has to be put on the socket or it
	// would not bound this call at all. It is cleared again once the stream is
	// the caller's: from there the proxy, not us, decides how long to wait.
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	fmt.Fprintf(conn, "GET /internal/tcp?port=%s HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n", port)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("nothing is listening on the %s port inside the sprite", port)
	}
	conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}
