package vercel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// meta is what Vercel knows about a sandbox beyond the engine's record, kept
// in Record.Ext["vercel"]. The engine never reads it; the session timeout it
// implies is mirrored into the engine's deadline rule (action stop), which is
// what acts on it. Times are milliseconds since the epoch, as on the wire.
type meta struct {
	Name       string            `json:"name"`
	ProjectID  string            `json:"projectId,omitempty"`
	Persistent bool              `json:"persistent"`
	Timeout    int64             `json:"timeout"` // ms: each new session's
	VCPUs      int               `json:"vcpus"`
	Memory     int               `json:"memory"` // MB
	Runtime    string            `json:"runtime"`
	Env        map[string]string `json:"env,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Routes     []route           `json:"routes,omitempty"`
	CreatedAt  int64             `json:"createdAt"`
	UpdatedAt  int64             `json:"updatedAt"`
	// StatusUpdatedAt is when the current session last changed status.
	StatusUpdatedAt int64 `json:"statusUpdatedAt"`
	// Sessions are the sandbox's sessions, oldest first; the last is current.
	// Older ones are kept (a few) so that a call on one answers 410, which is
	// what makes the SDKs resume, rather than 404.
	Sessions []session `json:"sessions"`
	// CurrentSnapshotID is what a resume starts from when the disk is not
	// already in that state (DiskIs): the last snapshot taken, by a stop of a
	// persistent sandbox or by POST …/snapshot.
	CurrentSnapshotID string `json:"currentSnapshotId,omitempty"`
	// DiskIs is the snapshot the sandbox's disk is identical to, "" once a
	// session has run on it since. A stop of a non-persistent sandbox discards
	// its filesystem: the disk is then no state at all, and only a restore of
	// CurrentSnapshotID (if there is one) can resume it.
	DiskIs    string     `json:"diskIs,omitempty"`
	Snapshots []snapshot `json:"snapshots,omitempty"`
	// TotalDurationMs adds up the sessions that have ended.
	TotalDurationMs int64 `json:"totalDurationMs,omitempty"`
}

// session is one run of the sandbox's VM.
type session struct {
	ID          string `json:"id"`
	RequestedAt int64  `json:"requestedAt"`
	StartedAt   int64  `json:"startedAt"`
	// Timeout is the session's, in ms from StartedAt: the sandbox's timeout
	// when it started, plus every extension.
	Timeout          int64  `json:"timeout"`
	Status           string `json:"status"` // running | stopped
	RequestedStopAt  int64  `json:"requestedStopAt,omitempty"`
	StoppedAt        int64  `json:"stoppedAt,omitempty"`
	SnapshottedAt    int64  `json:"snapshottedAt,omitempty"`
	SourceSnapshotID string `json:"sourceSnapshotId,omitempty"`
}

// deadline is when the session's timeout runs out.
func (s session) deadline() time.Time { return time.UnixMilli(s.StartedAt + s.Timeout) }

// route is a declared port and the subdomain that reaches it.
type route struct {
	Port      int    `json:"port"`
	Subdomain string `json:"subdomain"`
}

// snapshot is a snapshot of the sandbox's filesystem: one of the record's
// engine checkpoints.
type snapshot struct {
	ID              string `json:"id"`
	Checkpoint      string `json:"checkpoint"`
	SourceSessionID string `json:"sourceSessionId"`
	SizeBytes       int64  `json:"sizeBytes"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
	LastUsedAt      int64  `json:"lastUsedAt,omitempty"`
	// Method is "manual" (POST …/snapshot) or "stop" (a persistent sandbox's stop).
	Method string `json:"creationMethod"`
}

// maxSessions is how many sessions a sandbox remembers.
const maxSessions = 16

func (m *meta) current() *session {
	if len(m.Sessions) == 0 {
		return nil
	}
	return &m.Sessions[len(m.Sessions)-1]
}

func (m *meta) findSession(id string) *session {
	for i := range m.Sessions {
		if m.Sessions[i].ID == id {
			return &m.Sessions[i]
		}
	}
	return nil
}

func (m *meta) findSnapshot(id string) *snapshot {
	for i := range m.Snapshots {
		if m.Snapshots[i].ID == id {
			return &m.Snapshots[i]
		}
	}
	return nil
}

func (m *meta) addSession(s session) {
	m.Sessions = append(m.Sessions, s)
	if n := len(m.Sessions); n > maxSessions {
		m.Sessions = append([]session(nil), m.Sessions[n-maxSessions:]...)
	}
}

// metaOf reads a record's Vercel metadata; ok is false for a record that is
// not a Vercel sandbox.
func metaOf(rec store.Record) (meta, bool) {
	var m meta
	if rec.API != API {
		return m, false
	}
	raw, ok := rec.Ext[API]
	if !ok || json.Unmarshal(raw, &m) != nil || len(m.Sessions) == 0 {
		return m, false
	}
	return m, true
}

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
		m.UpdatedAt = f.ms()
		setMeta(r, m)
		r.UpdatedAt = f.now().UTC()
		out = m
	})
	return rec, out, err
}

func (f *Frontend) ms() int64 { return f.now().UnixMilli() }

// IDs, in hosted's shapes (observed): the SDKs treat them as opaque.
const (
	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
)

func randomString(alphabet string, n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, _ := rand.Int(rand.Reader, max)
		out[i] = alphabet[v.Int64()]
	}
	return string(out)
}

func newSessionID() string  { return "sbx_" + randomString(base62, 28) }
func newSnapshotID() string { return "snap_" + randomString(base62, 28) }
func newSubdomain() string  { return "sb-" + randomString(base36, 12) }
func newSandboxName() string {
	return "sandbox-" + randomString(base36, 10)
}

func newCommandID() string {
	b := make([]byte, 14)
	rand.Read(b)
	return "cmd_" + hex.EncodeToString(b)
}
