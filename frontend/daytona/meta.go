package daytona

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// meta is what Daytona knows about a sandbox beyond the engine's record, kept
// in Record.Ext["daytona"]. The engine never reads it; the auto-stop interval
// is mirrored into the engine's idle rule, which is what acts on it.
type meta struct {
	Name     string            `json:"name"`
	Labels   map[string]string `json:"labels,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	User     string            `json:"user"`
	Public   bool              `json:"public,omitempty"`
	Target   string            `json:"target"`
	Snapshot string            `json:"snapshot"`
	CPU      int               `json:"cpu"`
	MemGiB   int               `json:"memory"`
	GPU      int               `json:"gpu,omitempty"`
	// The lifecycle intervals, in minutes. AutoStop is enforced (the engine's
	// idle rule; 0 is never); AutoDelete is enforced for 0 only (an ephemeral
	// sandbox is deleted when it stops); the rest are kept and reported.
	AutoStop    int  `json:"autoStopInterval"`
	AutoArchive int  `json:"autoArchiveInterval"`
	AutoDelete  int  `json:"autoDeleteInterval"`
	AutoPause   *int `json:"autoPauseInterval,omitempty"`
	// NetworkBlockAll and NetworkAllowList are kept and reported, not enforced.
	NetworkBlockAll  bool   `json:"networkBlockAll,omitempty"`
	NetworkAllowList string `json:"networkAllowList,omitempty"`
	// PreviewToken is what a preview URL wants as x-daytona-preview-token
	// (unless the sandbox is public), and what the toolbox takes as
	// DAYTONA_SANDBOX_AUTH_KEY from clients that cannot send headers.
	PreviewToken string `json:"previewToken"`
	// Stopped is a stop through the API: the VM is down and only a start
	// brings it back; toolbox and preview traffic do not. (A sandbox whose VM
	// went down another way, its auto-stop say, is stopped too: state() asks
	// the engine.)
	Stopped bool `json:"stopped,omitempty"`
}

// metaOf reads a record's Daytona metadata; ok is false for a record that is
// not a Daytona sandbox.
func metaOf(rec store.Record) (meta, bool) {
	var m meta
	if rec.API != API {
		return m, false
	}
	raw, ok := rec.Ext[API]
	if !ok || json.Unmarshal(raw, &m) != nil {
		return m, false
	}
	return m, true
}

// setMeta writes m into rec's Ext.
func setMeta(rec *store.Record, m meta) {
	b, _ := json.Marshal(m)
	if rec.Ext == nil {
		rec.Ext = map[string]json.RawMessage{}
	}
	rec.Ext[API] = b
}

// updateMeta changes a sandbox's metadata in the store, atomically with
// respect to every other store write.
func (f *Frontend) updateMeta(id string, change func(*meta)) (store.Record, meta, error) {
	var out meta
	rec, err := f.store.UpdateRecord(id, func(r *store.Record) {
		m, _ := metaOf(*r)
		change(&m)
		setMeta(r, m)
		r.UpdatedAt = time.Now().UTC()
		out = m
	})
	return rec, out, err
}

// idlePolicy is the engine lifecycle policy for an auto-stop interval: stop
// cold after that many idle minutes, or never for 0. There is no deadline.
func idlePolicy(minutes int) store.LifecyclePolicy {
	if minutes <= 0 {
		return store.LifecyclePolicy{IdleAction: store.IdleNone}
	}
	return store.LifecyclePolicy{IdleAction: store.IdleStop, IdleTimeout: time.Duration(minutes) * time.Minute}
}

// newSandboxID is a random (version 4) UUID, as hosted Daytona's sandbox IDs are.
func newSandboxID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// newToken is a fresh preview token.
func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
