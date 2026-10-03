package e2b

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// meta is what E2B knows about a sandbox beyond the engine's record, kept in
// Record.Ext["e2b"]. The engine never reads it; the deadline it implies
// (EndAt, and what happens there) is mirrored into the engine's own deadline
// rule, which is what acts on it.
type meta struct {
	TemplateID string            `json:"templateID"`
	Alias      string            `json:"alias,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	EnvVars    map[string]string `json:"envVars,omitempty"`
	// AccessToken is envd's: the SDK sends it as X-Access-Token and signs file
	// URLs with it, and envd is given it by /init after every start.
	AccessToken string `json:"envdAccessToken"`
	// StartedAt is the create, or the last resume; EndAt the timeout's deadline,
	// or the moment of the pause.
	StartedAt time.Time `json:"startedAt"`
	EndAt     time.Time `json:"endAt"`
	// AutoPause pauses at EndAt rather than killing (onTimeout: "pause").
	AutoPause  bool `json:"autoPause,omitempty"`
	AutoResume bool `json:"autoResume,omitempty"`
	// Paused is a pause through the API (or at EndAt with AutoPause): the VM is
	// suspended and traffic does not wake it, only a connect does. A sandbox
	// that is not paused but whose VM is down (the daemon restarted) is still
	// running as far as E2B is concerned, and is woken by its traffic.
	Paused bool `json:"paused,omitempty"`
}

// paused is the sandbox's E2B state: paused through the API, or by its
// deadline passing with AutoPause (which the engine acts on, suspending the
// VM, without telling us).
func (m meta) paused(now time.Time) bool {
	return m.Paused || (m.AutoPause && !now.Before(m.EndAt))
}

func (m meta) state(now time.Time) string {
	if m.paused(now) {
		return "paused"
	}
	return "running"
}

// metaOf reads a record's E2B metadata; ok is false for a record that is not
// an E2B sandbox.
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

// idChars are what a sandbox ID is made of after its "i": E2B's
// id.Generate alphabet, which its proxy's Host parsing relies on.
const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// newSandboxID is "i" and 20 characters of [a-z0-9], as hosted E2B's are
// (handlers/sandbox_create.go, InstanceIDPrefix + id.Generate()).
func newSandboxID() string {
	b := make([]byte, 20)
	rand.Read(b)
	for i := range b {
		b[i] = idChars[int(b[i])%len(idChars)]
	}
	return "i" + string(b)
}

// newAccessToken is a fresh envd access token.
func newAccessToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
