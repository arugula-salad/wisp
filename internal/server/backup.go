package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/arugula-salad/wisp/internal/backup"
	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The backup tier runs entirely behind the lifecycle. A suspend is the natural
// moment: the guest has been told to sync and the VM is paused or gone, so the
// disk is a consistent point in time. Nothing here is allowed to make a suspend
// fail or a wake wait; see snapshot below for how that is kept true.
//
// NoBackupLabel opts a sprite out. Everything else with a configured bucket is
// backed up, because an opt-in default would leave most sprites with the
// durability this issue exists to fix.
const NoBackupLabel = "nobackup"

// backupSnapshotFile is the reflink clone a backup reads from, inside the machine
// directory (a reflink cannot cross filesystems). The leading dot keeps it out of
// the store's sprite listing, like the base-image mirror.
const backupSnapshotFile = ".backup.ext4"

type backupState struct {
	// Phase is idle, queued, running or error.
	Phase     string     `json:"phase"`
	Reason    string     `json:"reason,omitempty"`
	LastAt    *time.Time `json:"last_backup_at,omitempty"`
	LastTook  string     `json:"last_took,omitempty"`
	LastBytes int64      `json:"last_uploaded_bytes,omitempty"`
	// LastSize is what that backup protects: the data a restore would write.
	LastSize int64  `json:"last_backup_size_bytes,omitempty"`
	Error    string `json:"error,omitempty"`
	// Attempts since the last success, so a bucket that is down is visible as
	// something other than a single stale timestamp.
	Failures int `json:"failures,omitempty"`

	lastAttempt time.Time // paces the periodic loop's retries
}

// backupManager serialises backups: one sprite at a time, off the request path.
type backupManager struct {
	srv *Server
	cfg backup.Config

	// The bucket is opened on first use and again after every failure to, not once
	// at startup: a daemon that came up during an outage must not stay without
	// backups until someone restarts it.
	repoMu sync.Mutex
	repo   *backup.Repo

	mu        sync.Mutex
	state     map[string]*backupState // by sprite ID
	pending   map[string]string       // sprite ID -> reason
	order     []string                // pending IDs, oldest first
	wake      chan struct{}
	running   string // sprite ID of the job in flight
	bucketErr string // why the bucket cannot be opened; reported on every sprite
}

func newBackupManager(s *Server, cfg backup.Config) *backupManager {
	m := &backupManager{srv: s, cfg: cfg, state: map[string]*backupState{},
		pending: map[string]string{}, wake: make(chan struct{}, 1)}
	go m.run()
	// Reaching the bucket is not a condition of starting: wispd is still a
	// working sprite host without durability, and the error belongs in the log and
	// in sprite status, not in a refusal to boot or a slower one.
	go func() {
		m.repository(context.Background())
		if s.opts.Backup.Interval > 0 {
			m.periodic()
		}
	}()
	return m
}

// repository returns the open bucket, opening it if need be. The first time that
// works it also learns each sprite's recovery point from the bucket, so a restart
// neither forgets them nor re-reads every disk to find out nothing changed.
func (m *backupManager) repository(ctx context.Context) (*backup.Repo, error) {
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	if m.repo != nil {
		return m.repo, nil
	}
	log := m.srv.log
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repo, err := backup.Open(ctx, m.cfg)

	m.mu.Lock()
	was := m.bucketErr
	m.bucketErr = ""
	if err != nil {
		m.bucketErr = "backup bucket: " + err.Error()
	}
	m.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("backup bucket: %w", err)
		if m.bucketErr != was {
			log.Error("cannot open the backup bucket; sprites run on without backups until it answers",
				"bucket", m.cfg.Bucket, "endpoint", m.cfg.Endpoint, "err", err)
		}
		return nil, err
	}

	for _, sp := range m.srv.store.List("") {
		latest, err := repo.Latest(ctx, sp.ID)
		if err != nil {
			continue // never backed up, or unreadable: either way, no recovery point
		}
		m.mu.Lock()
		if st := m.stateFor(sp.ID); st.LastAt == nil {
			at := latest.CreatedAt
			st.LastAt, st.LastSize = &at, latest.Bytes()
		}
		m.mu.Unlock()
	}
	m.repo = repo
	log.Info("backups enabled", "bucket", m.cfg.Bucket, "endpoint", m.cfg.Endpoint,
		"encrypted", repo.Encrypted(), "interval", m.srv.opts.Backup.Interval)
	return repo, nil
}

// Enqueue asks for a backup of one sprite. It never blocks and never reports an
// error: a suspend that cannot be backed up is still a good suspend.
func (m *backupManager) Enqueue(sp store.Sprite, reason string) {
	if m == nil || slices.Contains(sp.Labels, NoBackupLabel) {
		return
	}
	m.mu.Lock()
	if _, dup := m.pending[sp.ID]; !dup {
		m.pending[sp.ID] = reason
		m.order = append(m.order, sp.ID)
		m.stateFor(sp.ID).Phase = "queued"
		m.stateFor(sp.ID).Reason = reason
	}
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// State is what the API reports for a sprite.
func (m *backupManager) State(id string) *backupState {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := backupState{Phase: "idle"}
	if st, ok := m.state[id]; ok {
		out = *st
	}
	// A bucket that cannot be opened is every sprite's problem, including the ones
	// that have not tried to use it yet.
	if m.bucketErr != "" && out.Error == "" && out.Phase == "idle" {
		out.Phase, out.Error = "error", m.bucketErr
	}
	return &out
}

// stateFor must be called with m.mu held.
func (m *backupManager) stateFor(id string) *backupState {
	st, ok := m.state[id]
	if !ok {
		st = &backupState{Phase: "idle"}
		m.state[id] = st
	}
	return st
}

func (m *backupManager) next() (id, reason string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.order) > 0 {
		id = m.order[0]
		m.order = m.order[1:]
		if reason, ok = m.pending[id]; ok {
			delete(m.pending, id)
			m.running = id
			m.stateFor(id).Phase = "running"
			return id, reason, true
		}
	}
	return "", "", false
}

func (m *backupManager) run() {
	for range m.wake {
		for {
			id, reason, ok := m.next()
			if !ok {
				break
			}
			m.one(id, reason)
			m.mu.Lock()
			m.running = ""
			m.mu.Unlock()
		}
	}
}

// one backs up a single sprite and records the outcome.
func (m *backupManager) one(id, reason string) {
	log := m.srv.log
	sp, err := m.findByID(id)
	if err != nil {
		return // deleted while it waited
	}
	// Generous: a first backup of a full 20 GB disk over a home network is slow,
	// and nothing is waiting on it.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	manifest, stats, err := m.backupSprite(ctx, sp, reason)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state[id]; !ok {
		// Deleted while it ran, and MarkDeleted dropped the state. If that backup
		// was the sprite's first, the tombstone was skipped for want of anything to
		// mark, so it is written now.
		if err == nil {
			go m.tombstone(sp)
		}
		return
	}
	st := m.stateFor(id)
	st.Reason, st.lastAttempt = reason, time.Now()
	switch {
	case errors.Is(err, errBackupDeferred), errors.Is(err, backup.ErrPruneRunning):
		st.Phase, st.Error = "idle", ""
		log.Info("backup deferred", "sprite", sp.Name, "why", err)
	case err != nil:
		st.Phase, st.Error = "error", err.Error()
		st.Failures++
		log.Warn("backup failed", "sprite", sp.Name, "err", err, "failures", st.Failures)
	default:
		// The recovery point is when the disk was captured, not when the upload
		// finished; the periodic loop compares it with the disk's mtime.
		at := manifest.CreatedAt
		st.Phase, st.Error, st.Failures = "idle", "", 0
		st.LastAt, st.LastTook = &at, stats.Took.Round(time.Millisecond).String()
		st.LastBytes, st.LastSize = stats.Uploaded, manifest.Bytes()
		log.Info("backup complete", "sprite", sp.Name, "reason", reason, "stats", stats.String())
	}
}

func (m *backupManager) findByID(id string) (store.Sprite, error) {
	for _, sp := range m.srv.store.List("") {
		if sp.ID == id {
			return sp, nil
		}
	}
	return store.Sprite{}, store.ErrNotFound
}

// errBackupDeferred means there was no consistent way to read the disk that does
// not get in a sprite's way right now. It is not a failure: the periodic loop
// tries again.
var errBackupDeferred = errors.New("backup deferred")

func (m *backupManager) backupSprite(ctx context.Context, sp store.Sprite, reason string) (*backup.Manifest, backup.Stats, error) {
	repo, err := m.repository(ctx)
	if err != nil {
		return nil, backup.Stats{}, err
	}
	c, err := m.capture(ctx, sp)
	if err != nil {
		return nil, backup.Stats{}, err
	}
	defer c.cleanup()

	opts := backup.SaveOptions{Reason: reason, At: c.at}
	if c.inPlace {
		// The disk is being read where a VM would write to it. A wake is never made
		// to wait for that: the upload is what gives way, as soon as it notices, and
		// in any case before it commits a manifest of a disk that moved under it.
		life := m.srv.life
		woke := fmt.Errorf("%w: the sprite woke during the upload", errBackupDeferred)
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		go func() {
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if life.diskGen(sp.ID) != c.gen {
						cancel(woke)
						return
					}
				}
			}
		}()
		opts.Precommit = func() error {
			if life.diskGen(sp.ID) != c.gen {
				return woke
			}
			return nil
		}
	}
	manifest, stats, err := repo.SaveWith(ctx, c.sprite, c.refs, opts)
	if cause := context.Cause(ctx); err != nil && errors.Is(cause, errBackupDeferred) {
		err = cause
	}
	return manifest, stats, err
}

// capture is a sprite frozen for a backup: its record, the files to read, and
// when they were frozen.
type capture struct {
	sprite  store.Sprite
	refs    []backup.FileRef
	at      time.Time
	inPlace bool   // the live disk is being read, not a clone of it
	gen     uint64 // the disk generation at capture, when inPlace
	cleanup func()
}

// capture picks a consistent point to read the sprite's disk from.
//
// With reflink support the clone is instant, so it is taken under the lifecycle
// lock (quiescing a running VM first, exactly as a checkpoint does) and the upload
// then runs with nothing held. Without reflinks there is no cheap consistent
// point: a running sprite is deferred rather than paused for the length of an
// upload, and a stopped one is read in place, by a caller that watches for it
// starting.
func (m *backupManager) capture(ctx context.Context, sp store.Sprite) (*capture, error) {
	dir := m.srv.store.Dir(sp.ID)
	live := filepath.Join(dir, vmm.DiskFile)
	rt := m.srv.life.rt(sp.ID)
	// Lock, not TryLock: this waits out a transition that is in flight, and holds
	// the lock only for an instant clone or for no work at all.
	rt.mu.Lock()
	defer rt.mu.Unlock()

	c := &capture{at: time.Now(), cleanup: func() {}}
	diskPath := live
	if !m.srv.storage.reflink {
		if rt.m != nil {
			return nil, fmt.Errorf("%w: running, and this volume has no reflink support", errBackupDeferred)
		}
		c.inPlace, c.gen = true, rt.gen.Load()
	} else {
		if rt.m != nil {
			// Flush the guest's page cache, then freeze its vCPUs, so the clone is a
			// point in time the filesystem's journal can recover from.
			if err := agentCall(ctx, rt.m, http.MethodPost, "/internal/presuspend", nil, nil); err != nil {
				return nil, fmt.Errorf("sync guest filesystem: %w", err)
			}
			if err := rt.m.Pause(ctx); err != nil {
				return nil, fmt.Errorf("pause VM: %w", err)
			}
			defer func() {
				if rerr := rt.m.Resume(ctx); rerr != nil {
					m.srv.log.Error("resume after backup snapshot failed", "sprite", sp.Name, "err", rerr)
				}
			}()
			c.at = time.Now()
		}
		diskPath = filepath.Join(dir, backupSnapshotFile)
		if err := cloneReflink(live, diskPath); err != nil {
			return nil, fmt.Errorf("snapshot disk for backup: %w", err)
		}
		c.cleanup = func() { os.Remove(diskPath) }
	}

	// The record and the checkpoint list are read under the same lock a checkpoint
	// is taken under, so the manifest's two halves agree.
	var err error
	if c.sprite, err = m.srv.store.Get(sp.Name); err != nil {
		c.cleanup()
		return nil, err
	}
	if c.refs, err = backup.SpriteFiles(dir, diskPath, vmm.DiskFile); err != nil {
		c.cleanup()
		return nil, err
	}
	return c, nil
}

// periodic keeps a long-running sprite's recovery point from drifting, and is the
// retry for everything else: a backup that failed, one that was deferred, and one
// that was queued when wispd shut down.
func (m *backupManager) periodic() {
	every := m.srv.opts.Backup.Interval
	tick := min(max(every/10, time.Second), 5*time.Minute)
	for range time.Tick(tick) {
		if _, err := m.repository(context.Background()); err != nil {
			continue // State reports it; there is nothing to upload to
		}
		for _, sp := range m.srv.store.List("") {
			if slices.Contains(sp.Labels, NoBackupLabel) {
				continue
			}
			st := m.State(sp.ID)
			if st.Phase == "queued" || st.Phase == "running" {
				continue
			}
			// Back off while it keeps failing, up to the interval itself.
			if wait := min(tick<<min(st.Failures, 8), every); st.Failures > 0 && time.Since(st.lastAttempt) < wait {
				continue
			}
			// Without reflinks there is nothing consistent to read until it stops, and
			// its suspend will ask for a backup itself.
			running := m.srv.life.Status(sp) == "running"
			if running && !m.srv.storage.reflink {
				continue
			}
			if st.LastAt == nil {
				m.Enqueue(sp, "first")
				continue
			}
			// Only if something actually changed since that backup.
			disk, err := os.Stat(filepath.Join(m.srv.store.Dir(sp.ID), vmm.DiskFile))
			if err != nil || !disk.ModTime().After(*st.LastAt) {
				continue
			}
			if !running {
				// A stopped sprite whose disk is newer than its recovery point is a
				// backup that was missed, whatever the reason.
				m.Enqueue(sp, "catch-up")
			} else if time.Since(*st.LastAt) >= every {
				m.Enqueue(sp, "periodic")
			}
		}
	}
}

// MarkDeleted records a tombstone so that a deleted sprite and a lost machine do
// not look the same to the bucket. Best effort, and off the request path.
func (m *backupManager) MarkDeleted(sp store.Sprite) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.pending, sp.ID)
	delete(m.state, sp.ID)
	m.mu.Unlock()
	go m.tombstone(sp)
}

func (m *backupManager) tombstone(sp store.Sprite) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, err := m.repository(ctx)
	if err == nil {
		// A sprite that was never backed up has nothing in the bucket to mark.
		var stamps []string
		if stamps, err = repo.Stamps(ctx, sp.ID); err == nil && len(stamps) > 0 {
			err = repo.MarkDeleted(ctx, sp.ID, sp.Name)
		}
	}
	if err != nil {
		m.srv.log.Warn("could not record the deletion in the backup bucket",
			"sprite", sp.Name, "err", err)
	}
}
