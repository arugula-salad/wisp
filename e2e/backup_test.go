//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// The backup tier as the API shows it: a suspend produces a recovery point, the
// second backup of an unchanged sprite moves nothing, and a sprite labelled
// nobackup is left alone. Whole-machine loss and restore needs a second data
// directory and a restart, which scripts/verify-backup.sh does; this suite checks
// everything that can be seen through the running daemon.
//
// Needs spritesd started with a reachable --backup-bucket. Skipped otherwise.

type backupStatus struct {
	Phase     string     `json:"phase"`
	Reason    string     `json:"reason"`
	LastAt    *time.Time `json:"last_backup_at"`
	LastBytes int64      `json:"last_uploaded_bytes"`
	LastSize  int64      `json:"last_backup_size_bytes"`
	Error     string     `json:"error"`
	Failures  int        `json:"failures"`
}

// backupOf reads the non-upstream `backup` object out of the sprite JSON.
func backupOf(t *testing.T, name string) backupStatus {
	t.Helper()
	body := want(t, "GET", "/v1/sprites/"+name, "", 200)
	var sp struct {
		Backup *backupStatus `json:"backup"`
	}
	if err := json.Unmarshal([]byte(body), &sp); err != nil {
		t.Fatalf("decode sprite: %v: %s", err, body)
	}
	if sp.Backup == nil {
		t.Skip("spritesd is running without --backup-bucket")
	}
	return *sp.Backup
}

// backedUpAfter waits for a recovery point newer than since.
func backedUpAfter(t *testing.T, name string, since time.Time, d time.Duration) backupStatus {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(time.Second) {
		st := backupOf(t, name)
		if st.Error != "" {
			t.Fatalf("backup of %s failed: %s", name, st.Error)
		}
		if st.LastAt != nil && st.LastAt.After(since) {
			return st
		}
	}
	t.Fatalf("no backup of %s within %s: %+v", name, d, backupOf(t, name))
	return backupStatus{}
}

func TestBackupTier(t *testing.T) {
	c := client(t)
	idle := idleTimeout(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	name := fmt.Sprintf("e2e-backup-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })
	backupOf(t, name) // skips the whole suite if backups are off

	// Write something worth keeping, then let it suspend.
	if out, err := sp.CommandContext(ctx, "sh", "-c", "echo durable > ~/proof.txt && sync").CombinedOutput(); err != nil {
		t.Fatalf("write: %v: %s", err, out)
	}
	cs, err := sp.CreateCheckpointWithComment(ctx, "before the backup")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := cs.ProcessAll(func(*sprites.StreamMessage) error { return nil }); err != nil {
		t.Fatalf("checkpoint stream: %v", err)
	}

	start := time.Now()
	suspendsWithin(t, c, name, idle+15*time.Second)
	first := backedUpAfter(t, name, start, 10*time.Minute)
	t.Logf("first backup: %d bytes uploaded of %d held, reason %q", first.LastBytes, first.LastSize, first.Reason)
	if first.Reason != "suspend" {
		t.Errorf("first backup reason = %q, want suspend", first.Reason)
	}
	if first.LastBytes == 0 {
		t.Error("the first backup of a fresh sprite uploaded nothing")
	}

	t.Run("wake is not blocked by a backup", func(t *testing.T) {
		// A wake right after a suspend competes with the upload; it must not wait for
		// it. Cold boot is ~200 ms and warm restore ~15 ms, so a couple of seconds is
		// generous and still fails loudly if the upload is in the way.
		woke := time.Now()
		if out, err := sp.CommandContext(ctx, "sh", "-c", "cat ~/proof.txt").Output(); err != nil ||
			string(out) != "durable\n" {
			t.Fatalf("wake and read: %q %v", out, err)
		}
		if took := time.Since(woke); took > 20*time.Second {
			t.Errorf("waking a sprite mid-backup took %s", took)
		}
	})

	t.Run("second backup of unchanged data transfers almost nothing", func(t *testing.T) {
		before := time.Now()
		suspendsWithin(t, c, name, idle+15*time.Second)
		second := backedUpAfter(t, name, before, 10*time.Minute)
		t.Logf("second backup: %d bytes uploaded", second.LastBytes)
		// Reading a file is not a write, but a wake does touch the filesystem, so the
		// bar is "a small fraction of the disk", not zero. Of the disk, not of the
		// first upload: in a bucket that already holds this base image, the first
		// upload is mostly deduplicated as well.
		if second.LastSize == 0 || second.LastBytes > second.LastSize/4 {
			t.Errorf("second backup uploaded %d bytes of the %d it holds: it is not incremental",
				second.LastBytes, second.LastSize)
		}
	})

	t.Run("nobackup opts a sprite out", func(t *testing.T) {
		skipped := name + "-skip"
		if _, err := c.CreateSpriteWithOrg(ctx, skipped, nil, nil, []string{"nobackup"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { c.DeleteSprite(context.Background(), skipped) })
		suspendsWithin(t, c, skipped, idle+30*time.Second)
		time.Sleep(5 * time.Second) // long enough for a backup to have started
		if st := backupOf(t, skipped); st.LastAt != nil {
			t.Errorf("a sprite labelled nobackup was backed up: %+v", st)
		}
	})
}
