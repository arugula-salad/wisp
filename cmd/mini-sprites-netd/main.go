// mini-sprites-netd is the one privileged piece of network policy: a root
// service that lets the unprivileged spritesd replace the membership of the
// nftables set `inet mini_sprites restricted4`, and nothing else. Installed and
// started by scripts/setup-host.sh.
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

	"github.com/jhgaylor/mini-sprites/internal/netd"
)

func main() {
	socket := flag.String("socket", netd.DefaultSocket, "unix socket to listen on")
	owner := flag.String("owner", "", "user that runs spritesd; the socket is theirs alone")
	network := flag.String("net", "", "sprite network, e.g. 10.209.0.0/16; addresses outside it are refused")
	nft := flag.String("nft", "/usr/sbin/nft", "nft binary")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *socket, *owner, *network, *nft); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger, socket, owner, network, nft string) error {
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
	log.Info("mini-sprites-netd listening", "socket", socket, "owner", owner, "net", prefix)
	srv := &netd.Server{Net: prefix.Masked(), OwnerUID: uid, Apply: netd.NftApply(nft), Log: log}
	return srv.Serve(ln)
}
