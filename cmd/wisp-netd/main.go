// wisp-netd is the one privileged piece of network policy: a root
// service that lets the unprivileged wispd replace the membership of the
// nftables set `inet wisp restricted4` (`inet wispN restricted4` for --pool N),
// and nothing else. Installed and started by scripts/setup-host.sh.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/arugula-salad/wisp/internal/netd"
)

func main() {
	pool := flag.Int("pool", 0, "network pool whose set this helper edits: inet wisp restricted4 for 0, inet wispN restricted4 for N")
	socket := flag.String("socket", "", "unix socket to listen on (default /run/wisp/netd.sock, /run/wispN/netd.sock for --pool N)")
	owner := flag.String("owner", "", "user that runs wispd; the socket is theirs alone")
	network := flag.String("net", "", "sprite network, e.g. 10.209.0.0/16; addresses outside it are refused")
	nft := flag.String("nft", "/usr/sbin/nft", "nft binary")
	flag.Parse()
	if *pool < 0 {
		fmt.Fprintln(os.Stderr, "--pool must not be negative")
		os.Exit(2)
	}
	if *socket == "" {
		*socket = netd.Pool(*pool).Socket()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, netd.Pool(*pool), *socket, *owner, *network, *nft); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger, pool netd.Pool, socket, owner, network, nft string) error {
	prefix, err := netip.ParsePrefix(network)
	if err != nil || !prefix.Addr().Is4() {
		return fmt.Errorf("--net must be an IPv4 prefix, got %q", network)
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return fmt.Errorf("--owner: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	os.Remove(socket) // left behind by an unclean exit
	// Created 0600 from the start: between bind and chown it is root's, never the world's.
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return err
	}
	if err := os.Chown(socket, uid, gid); err != nil {
		return err
	}
	log.Info("wisp-netd listening", "socket", socket, "owner", owner, "net", prefix, "set", pool.Set())
	srv := &netd.Server{Pool: pool, Net: prefix.Masked(), OwnerUID: uid, Apply: netd.NftApply(nft), Log: log}
	return srv.Serve(ln)
}
