//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestExecPostChunkFraming checks the one property upstream's HTTP exec format
// depends on and an ordinary HTTP client hides: frames carry no length, so each
// must arrive as exactly one HTTP chunk. It speaks HTTP/1.1 by hand to see the
// chunk boundaries, the way upstream's JS SDK sees them through fetch().
func TestExecPostChunkFraming(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-post-%d", time.Now().UnixNano()%1e9)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	base, _ := url.Parse(e2eURL())
	conn, err := net.Dial("tcp", base.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(90 * time.Second))

	// Interleaved streams, bursts of tiny writes that invite coalescing, and one
	// write far larger than any buffer on the path, which invites splitting.
	script := `for i in 1 2 3 4 5; do echo out$i; echo err$i >&2; done; head -c 300000 /dev/zero | tr '\0' x; cat; exit 9`
	q := url.Values{"cmd": {"sh", "-c", script}, "stdin": {"true"}}
	stdin := "from-stdin"
	fmt.Fprintf(conn, "POST /v1/sprites/%s/exec?%s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
		name, q.Encode(), base.Host, e2eToken(), len(stdin), stdin)

	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "200") {
		rest, _ := io.ReadAll(br)
		t.Fatalf("status %q: %s", strings.TrimSpace(status), rest)
	}
	chunked := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.Contains(strings.ToLower(line), "transfer-encoding: chunked") {
			chunked = true
		}
	}
	if !chunked {
		t.Fatal("response is not chunked, so frames cannot be delimited at all")
	}

	var stdout, stderr strings.Builder
	exit, frames := -1, 0
	for {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading chunk size: %v", err)
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil {
			t.Fatalf("bad chunk size %q", sizeLine)
		}
		if size == 0 {
			break
		}
		chunk := make([]byte, size+2) // payload + CRLF
		if _, err := io.ReadFull(br, chunk); err != nil {
			t.Fatal(err)
		}
		frame := chunk[:size]
		frames++
		if exit != -1 {
			t.Fatalf("frame after the exit frame: type %d", frame[0])
		}
		switch frame[0] {
		case 1:
			stdout.Write(frame[1:])
		case 2:
			stderr.Write(frame[1:])
		case 3:
			if len(frame) != 2 {
				t.Fatalf("exit frame is %d bytes; upstream clients require exactly 2", len(frame))
			}
			exit = int(frame[1])
		default:
			t.Fatalf("chunk %d starts with byte %#x: not a frame boundary (coalesced or split)", frames, frame[0])
		}
	}
	wantOut := "out1\nout2\nout3\nout4\nout5\n" + strings.Repeat("x", 300000) + "from-stdin"
	if stdout.String() != wantOut {
		t.Fatalf("stdout: got %d bytes, want %d (a frame byte leaking into the payload means a boundary was lost)", stdout.Len(), len(wantOut))
	}
	if stderr.String() != "err1\nerr2\nerr3\nerr4\nerr5\n" || exit != 9 {
		t.Fatalf("stderr=%q exit=%d", stderr.String(), exit)
	}
	t.Logf("%d frames, each its own chunk", frames)
}
