//go:build netns

// The kernel half of network policy, exercised for real without root on the
// host: scripts/test-netpolicy-netns.sh runs this test binary as "root" inside a
// rootless container, where it owns a private network namespace. It builds the
// msbr0 bridge, loads the exact ruleset setup-host.sh installs, runs the real
// wisp-netd against the real nft, stands up wispd's policy listeners,
// and plays the part of two sprites with two further namespaces.
//
// Not covered here (needs the real host): ufw, tap devices and bridge port
// isolation, the systemd unit, and an actual Firecracker guest.
package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhgaylor/wisp/internal/store"
)

func sh(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("sh", "-ec", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s\n%v: %s", script, err, out)
	}
	return string(out)
}

// in runs a command inside a sprite's namespace and reports whether it succeeded.
// nsenter rather than "ip netns exec", which wants to remount /sys and may not in
// a rootless container.
func in(ns, script string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nsenter", "--net=/run/netns/"+ns, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *syncBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func TestNetworkPolicyInNamespaces(t *testing.T) {
	setup, netdBin := os.Getenv("MS_SETUP"), os.Getenv("MS_NETD")
	if setup == "" || netdBin == "" || os.Getuid() != 0 {
		t.Skip("run via scripts/test-netpolicy-netns.sh")
	}
	sh(t, `ip link add msbr0 type bridge && ip addr add 10.209.0.1/16 dev msbr0 && ip link set msbr0 up`)
	for ns, ip := range map[string]string{"shut": "10.209.0.2", "open": "10.209.0.3"} {
		sh(t, fmt.Sprintf(`ip netns add %[1]s
			ip link add veth-%[1]s type veth peer name eth0 netns %[1]s
			ip link set veth-%[1]s master msbr0 up
			nsenter --net=/run/netns/%[1]s sh -ec 'ip link set lo up; ip link set eth0 up
				ip addr add %[2]s/16 dev eth0; ip route add default via 10.209.0.1'
			bridge link set dev veth-%[1]s isolated on`, ns, ip))
		if out, ok := in(ns, "ip -4 -o addr show dev eth0"); !ok || !strings.Contains(out, ip) {
			t.Fatalf("cannot run commands in namespace %s: %s", ns, out)
		}
	}
	// What a guest is given. Only the fake sprites resolve through this file; everything
	// wispd-side in this test dials addresses.
	sh(t, `echo 'nameserver 1.1.1.1' > /etc/resolv.conf`)
	sh(t, setup+" --print-rules | nft -f -")

	socket := "/run/wisp/netd.sock"
	helper := exec.Command(netdBin, "--owner", "root", "--net", "10.209.0.0/16", "--socket", socket)
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer helper.Process.Kill()
	for i := 0; ; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		} else if i > 100 {
			t.Fatal("helper socket never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs), nil))
	shut, open := addSprite(t, st, "shut"), addSprite(t, st, "open") // net_index 2 and 3, matching the namespaces
	e := newEgress(Options{DNS: "1.1.1.1,8.8.8.8", NetdSocket: socket}, st, log, net.IPv4(10, 209, 0, 1), nil)
	if e.down != "" {
		t.Fatalf("listeners: %s", e.down)
	}
	s := &Server{store: st, life: &Lifecycle{store: st, log: log, runtimes: map[string]*runtime{}, egress: e}, log: log, token: "t"}
	for _, sp := range []store.Sprite{shut, open} { // what startLocked does before attaching a NIC
		if err := e.admit(sp); err != nil {
			t.Fatal(err)
		}
	}
	setPolicy := func(body string) {
		t.Helper()
		if w := call(s, "POST", "/v1/sprites/shut/policy/network", body); w.Code != 204 {
			t.Fatalf("set policy: %d %s", w.Code, w.Body)
		}
	}
	members := func() string {
		return strings.Join(strings.Fields(sh(t, "nft list set inet wisp restricted4 | grep elements || true")), " ")
	}

	const fetch = `curl -sS -m 15 -o /dev/null -w '%%{http_code}' https://%s/`
	check := func(name string, ok bool, detail string) {
		t.Helper()
		if ok {
			t.Logf("PASS  %s", name)
		} else {
			t.Errorf("FAIL  %s: %s", name, detail)
		}
	}
	// control reports whether something works from the unrestricted namespace; where
	// the container's own network cannot do it at all, the restricted result proves nothing.
	control := func(name, script string) bool {
		out, ok := in("open", script)
		if !ok {
			t.Logf("SKIP  %s: not possible from this container even unrestricted (%s)", name, out)
		}
		return ok
	}

	out, ok := in("shut", fmt.Sprintf(fetch, "github.com"))
	check("before any policy the sprite reaches the internet", ok, out)

	// A connection opened on the plain NAT path, before there was a policy. It even
	// goes to a domain the policy will allow, but the policy never vetted it.
	const lateRequest = `bash -c 'exec 3<>/dev/tcp/example.com/80 || exit 0; sleep %d; printf "GET / HTTP/1.0\r\nHost: example.com\r\n\r\n" >&3; timeout 5 head -c 12 <&3'`
	out, _ = in("shut", fmt.Sprintf(lateRequest, 0))
	check("(control) a held-open connection can make a late request", strings.HasPrefix(out, "HTTP/"), out)
	preexisting := make(chan string, 1)
	go func() {
		out, _ := in("shut", fmt.Sprintf(lateRequest, 4))
		preexisting <- out
	}()
	time.Sleep(2 * time.Second)

	setPolicy(`{"rules":[{"domain":"example.com","action":"allow"},{"domain":"*.nip.io","action":"allow"}]}`)
	out = <-preexisting
	check("a connection opened before the policy stops working once it is set", !strings.Contains(out, "HTTP/"), out)
	check("the kernel set holds the restricted sprite", members() == "elements = { 10.209.0.2 }", members())

	out, ok = in("shut", fmt.Sprintf(fetch, "example.com"))
	check("allowed domain works over TLS through the proxy", ok && strings.HasPrefix(out, "2"), out)

	out, _ = in("shut", "dig +time=3 +tries=1 github.com")
	check("denied domain is REFUSED at DNS", strings.Contains(out, "status: REFUSED"), out)
	out, ok = in("shut", fmt.Sprintf(fetch, "github.com"))
	check("denied domain cannot be fetched", !ok, out)

	ghIP, _ := in("open", "dig +short github.com A | grep -E '^[0-9.]+$' | head -1")
	out, ok = in("shut", fmt.Sprintf("curl -sS -m 10 -o /dev/null --resolve github.com:443:%s https://github.com/", ghIP))
	check("denied domain cannot be reached by IP ("+ghIP+")", ghIP != "" && !ok, out)

	// dig reports timeouts on stdout, so "got an answer" means "printed an address".
	const gotAddr = ` | grep -Eq '^[0-9]+(\.[0-9]+){3}$'`
	out, ok = in("shut", "dig +time=3 +tries=1 +short @8.8.8.8 github.com"+gotAddr)
	check("naming another resolver changes nothing", !ok, out)

	out, _ = in("shut", "dig +time=3 +tries=1 10.0.0.1.nip.io A")
	check("rebinding: private answer for an allowed name is stripped", strings.Contains(out, "status: NOERROR") && strings.Contains(out, "ANSWER: 0"), out)
	out, _ = in("shut", "dig +time=3 +tries=1 +short 1.1.1.1.nip.io A")
	check("...while a public answer for the same wildcard passes", out == "1.1.1.1", out)

	if control("udp", "dig +time=3 +tries=1 +short @9.9.9.9 -p 9953 example.com"+gotAddr) {
		out, ok = in("shut", "dig +time=3 +tries=1 +short @9.9.9.9 -p 9953 example.com"+gotAddr)
		check("UDP other than DNS is dropped", !ok, out)
	}
	if control("icmp", "ping -c1 -W3 1.1.1.1") {
		out, ok = in("shut", "ping -c1 -W3 1.1.1.1")
		check("ICMP is dropped", !ok, out)
	}

	hostIP := strings.TrimSpace(sh(t, `ip -4 -o addr show scope global | grep -v msbr0 | awk '{print $4}' | cut -d/ -f1 | head -1`))
	ln, err := net.Listen("tcp4", hostIP+":8099") // a "host service" on the host's LAN address
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var reached atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			c.Close()
		}
	}()
	// These fail for unrestricted sprites too (by the forward chain), so failure alone
	// proves little. What matters is that the proxy, which dials from the host and is
	// past that chain, saw each attempt and refused it itself. connect() does succeed:
	// a transparent proxy has to accept before it can learn where the sprite was going.
	for _, target := range []string{hostIP + ":8099", "10.209.0.1:8099", "169.254.169.254:80", "100.100.100.100:80", "10.88.0.1:80"} {
		host, port, _ := strings.Cut(target, ":")
		out, ok = in("shut", "echo hi | nc -w3 "+host+" "+port)
		denied := strings.Contains(logs.String(), `dst=`+target+` reason="non-public or host address"`)
		check("private/host address "+target+" is refused by the proxy", denied && out == "", fmt.Sprintf("denial-logged=%v output=%q", denied, out))
	}
	check("the host service saw no connection", reached.Load() == 0, fmt.Sprint(reached.Load()))
	out, ok = in("open", "nc -z -w3 10.209.0.1 7880")
	check("policy proxy port is closed to unrestricted sprites", !ok, out)
	out, ok = in("open", "dig +time=2 +tries=1 +short @10.209.0.1 -p 7853 github.com"+gotAddr)
	check("policy DNS port is closed to unrestricted sprites", !ok, out)

	out, ok = in("open", fmt.Sprintf(fetch, "github.com"))
	check("unrestricted sprite is unaffected", ok, out)

	// Tightening kills what is already open: hold a connection to example.com, then drop it from the policy.
	held := make(chan error, 1)
	go func() {
		// read's status is >128 only if it timed out, i.e. the connection was still open.
		_, ok := in("shut", `bash -c 'exec 3<>/dev/tcp/example.com/80 || exit 0; read -t 8 line <&3; [ $? -gt 128 ]'`)
		if ok {
			held <- fmt.Errorf("connection survived")
		} else {
			held <- nil
		}
	}()
	time.Sleep(3 * time.Second)
	setPolicy(`{"rules":[{"domain":"github.com","action":"allow"}]}`)
	check("live connection to a newly denied domain is closed", <-held == nil, "still open 8s after the policy change")
	check("...by the proxy, because of the change", strings.Contains(logs.String(), `msg="egress connection closed by policy change" sprite=shut domain=example.com`), "no such log line")
	out, ok = in("shut", fmt.Sprintf(fetch, "github.com"))
	check("replacement policy is live: newly allowed domain works", ok, out)
	out, ok = in("shut", fmt.Sprintf(fetch, "example.com"))
	check("replacement policy is live: previously allowed domain fails", !ok, out)

	setPolicy(`{"rules":[]}`)
	check("clearing the policy empties the kernel set", members() == "", members())
	out, ok = in("shut", fmt.Sprintf(fetch, "example.com"))
	check("clearing the policy restores access live", ok, out)
	if control("icmp", "ping -c1 -W3 1.1.1.1") {
		out, ok = in("shut", "ping -c1 -W3 1.1.1.1")
		check("...including ICMP", ok, out)
	}

	// A set emptied behind wispd's back (setup-host.sh re-run without KEEP, nft flush) is repaired at the next boot.
	setPolicy(`{"rules":[{"domain":"example.com","action":"allow"}]}`)
	sh(t, "nft flush set inet wisp restricted4")
	if err := e.admit(shut); err != nil {
		t.Fatal(err)
	}
	check("a boot re-pushes the set", members() == "elements = { 10.209.0.2 }", members())
	// Re-applying the ruleset the way setup-host.sh does keeps the members.
	sh(t, `KEEP=$(nft list set inet wisp restricted4 | tr -d '\n\t' | sed -n 's/.*elements = {\([^}]*\)}.*/\1/p'); test -n "$KEEP"; KEEP="$KEEP" `+setup+` --print-rules | nft -f -`)
	check("re-applying the ruleset preserves the restricted set", members() == "elements = { 10.209.0.2 }", members())

	helper.Process.Kill()
	helper.Wait()
	setPolicy(`{"rules":[]}`) // loosening needs no helper
	w := call(s, "POST", "/v1/sprites/shut/policy/network", `{"rules":[{"domain":"example.com","action":"allow"}]}`)
	check("with the helper dead a restrictive policy is refused (503)", w.Code == 503 && strings.Contains(w.Body.String(), "policy_unenforceable"), w.Body.String())
}
