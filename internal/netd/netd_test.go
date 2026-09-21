package netd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var spriteNet = netip.MustParsePrefix("10.209.0.0/16")

func TestScript(t *testing.T) {
	got, err := Script(spriteNet, []string{"10.209.0.3", "10.209.1.7", "10.209.0.3"})
	if err != nil {
		t.Fatal(err)
	}
	want := "flush set inet mini_sprites restricted4\n" +
		"add element inet mini_sprites restricted4 { 10.209.0.3, 10.209.1.7 }\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Emptying the set is a flush alone: "add element { }" is a syntax error in nft.
	if got, err := Script(spriteNet, nil); err != nil || got != "flush set inet mini_sprites restricted4\n" {
		t.Errorf("empty: %q, %v", got, err)
	}
}

func TestScriptRejects(t *testing.T) {
	for name, members := range map[string][]string{
		"outside the network":   {"10.210.0.2"},
		"public address":        {"10.209.0.2", "8.8.8.8"},
		"the host's LAN":        {"192.168.1.10"},
		"the gateway":           {"10.209.0.1"},
		"the network address":   {"10.209.0.0"},
		"a prefix":              {"10.209.0.0/24"},
		"a range":               {"10.209.0.2-10.209.0.9"},
		"ipv6":                  {"fd00::2"},
		"v4-mapped":             {"::ffff:10.209.0.2"},
		"empty string":          {""},
		"a hostname":            {"sprite.local"},
		"nft injection":         {"10.209.0.2 }; flush ruleset; add element inet mini_sprites restricted4 { 10.209.0.3"},
		"newline injection":     {"10.209.0.2\nflush ruleset"},
		"trailing garbage":      {"10.209.0.2,"},
		"zone":                  {"10.209.0.2%eth0"},
		"too many":              make([]string, maxAddrs+1),
		"leading zeros (octal)": {"010.209.0.2"},
	} {
		if script, err := Script(spriteNet, members); err == nil {
			t.Errorf("%s: accepted, producing %q", name, script)
		}
	}
}

type fakeNft struct {
	mu      sync.Mutex
	scripts []string
	err     error
}

func (f *fakeNft) apply(script string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	return f.err
}

func (f *fakeNft) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.scripts...)
}

func serve(t *testing.T, ownerUID int, nft *fakeNft) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "netd.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &Server{Net: spriteNet, OwnerUID: ownerUID, Apply: nft.apply, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go srv.Serve(ln)
	return socket
}

func push(socket string, addrs ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var parsed []netip.Addr
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	return Push(ctx, socket, parsed)
}

func TestPushReplacesMembership(t *testing.T) {
	nft := &fakeNft{}
	socket := serve(t, os.Getuid(), nft)
	if err := push(socket, "10.209.0.2", "10.209.0.3"); err != nil {
		t.Fatal(err)
	}
	if err := push(socket); err != nil {
		t.Fatal(err)
	}
	calls := nft.calls()
	if len(calls) != 2 || !strings.Contains(calls[0], "{ 10.209.0.2, 10.209.0.3 }") || strings.Contains(calls[1], "add element") {
		t.Errorf("nft saw %q", calls)
	}
}

func TestPushOutsideNetworkNeverReachesNft(t *testing.T) {
	nft := &fakeNft{}
	socket := serve(t, os.Getuid(), nft)
	err := push(socket, "10.209.0.2", "192.168.1.1")
	if err == nil || !strings.Contains(err.Error(), "outside the sprite network") {
		t.Errorf("err = %v", err)
	}
	if len(nft.calls()) != 0 {
		t.Errorf("a rejected request still ran nft: %q", nft.calls())
	}
}

func TestPushReportsNftFailure(t *testing.T) {
	nft := &fakeNft{err: errors.New("Error: No such file or directory; did you mean table 'x'?")}
	socket := serve(t, os.Getuid(), nft)
	if err := push(socket, "10.209.0.2"); err == nil || !strings.Contains(err.Error(), "No such file") {
		t.Errorf("err = %v", err)
	}
}

func TestOtherUsersAreRefused(t *testing.T) {
	nft := &fakeNft{}
	socket := serve(t, os.Getuid()+1, nft) // the socket's owner is somebody else
	if err := push(socket, "10.209.0.2"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("err = %v", err)
	}
	if len(nft.calls()) != 0 {
		t.Error("a request from the wrong uid ran nft")
	}
}

func TestMalformedRequests(t *testing.T) {
	nft := &fakeNft{}
	socket := serve(t, os.Getuid(), nft)
	for name, body := range map[string]string{
		"not json":        "flush ruleset\n",
		"unknown field":   `{"restricted4":[],"table":"filter"}` + "\n",
		"wrong type":      `{"restricted4":"10.209.0.2"}` + "\n",
		"another command": `{"exec":"/bin/sh"}` + "\n",
	} {
		c, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(body))
		reply, _ := io.ReadAll(c)
		c.Close()
		if !strings.Contains(string(reply), `"ok":false`) {
			t.Errorf("%s: reply %q", name, reply)
		}
	}
	if len(nft.calls()) != 0 {
		t.Errorf("malformed requests ran nft: %q", nft.calls())
	}
}

func TestPushWithoutHelper(t *testing.T) {
	if err := push(filepath.Join(t.TempDir(), "absent.sock"), "10.209.0.2"); err == nil {
		t.Error("pushing to a socket nobody listens on must fail")
	}
}

func TestNftApplyFeedsStdin(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "got")
	fake := filepath.Join(dir, "nft")
	os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1\" = -f ] && [ \"$2\" = - ] || exit 9\ncat > "+out+"\n"), 0o755)
	if err := NftApply(fake)("flush set inet mini_sprites restricted4\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "flush set inet mini_sprites restricted4\n" {
		t.Errorf("nft got %q", b)
	}
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'Error: no such table' >&2; exit 1\n"), 0o755)
	if err := NftApply(fake)("x"); err == nil || !strings.Contains(err.Error(), "no such table") {
		t.Errorf("err = %v", err)
	}
}
