package netd

import (
	"strings"
	"testing"
)

func TestPoolNames(t *testing.T) {
	// Pool 0 is every existing install: its names must never change.
	for _, c := range []struct {
		pool                           Pool
		bridge, taps, table, set, sock string
	}{
		{0, "msbr0", "mstap", "wisp", "inet wisp restricted4", "/run/wisp/netd.sock"},
		{1, "msbr1", "ms1tap", "wisp1", "inet wisp1 restricted4", "/run/wisp1/netd.sock"},
		{12, "msbr12", "ms12tap", "wisp12", "inet wisp12 restricted4", "/run/wisp12/netd.sock"},
	} {
		if got := c.pool.Bridge(); got != c.bridge {
			t.Errorf("pool %d bridge %q, want %q", c.pool, got, c.bridge)
		}
		if got := c.pool.TapPrefix(); got != c.taps {
			t.Errorf("pool %d tap prefix %q, want %q", c.pool, got, c.taps)
		}
		if got := c.pool.Table(); got != c.table {
			t.Errorf("pool %d table %q, want %q", c.pool, got, c.table)
		}
		if got := c.pool.Set(); got != c.set {
			t.Errorf("pool %d set %q, want %q", c.pool, got, c.set)
		}
		if got := c.pool.Socket(); got != c.sock {
			t.Errorf("pool %d socket %q, want %q", c.pool, got, c.sock)
		}
	}
}

// A wispd finds its taps by prefix, so no pool's prefix may start another's names.
func TestPoolTapPrefixesDisjoint(t *testing.T) {
	for a := Pool(0); a < 20; a++ {
		for b := Pool(0); b < 20; b++ {
			if a != b && strings.HasPrefix(b.TapPrefix()+"0", a.TapPrefix()) {
				t.Errorf("pool %d's taps (%s0) look like pool %d's (%s*)", b, b.TapPrefix(), a, a.TapPrefix())
			}
		}
	}
}
