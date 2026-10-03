package modal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/internal/store"
)

// meta is what Modal knows about a sandbox beyond the engine's record, kept
// in Record.Ext["modal"]. The engine never reads it; the deadline (Timeout
// after create) is mirrored into the engine's own deadline rule, which acts on it.
type meta struct {
	AppID   string `json:"appID"`
	TaskID  string `json:"taskID"`
	ImageID string `json:"imageID"`
	// Entrypoint is the sandbox's command; the sandbox ends when it does.
	Entrypoint []string `json:"entrypoint,omitempty"`
	// Workdir and Env are the container's: execs start from them.
	Workdir  string            `json:"workdir,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Deadline time.Time         `json:"deadline"`
}

// metaOf reads a record's Modal metadata; ok is false for a record that is
// not a Modal sandbox.
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

func setMeta(rec *store.Record, m meta) {
	b, _ := json.Marshal(m)
	if rec.Ext == nil {
		rec.Ext = map[string]json.RawMessage{}
	}
	rec.Ext[API] = b
}

// IDs. The client decides V1 or V2 from a sandbox ID's shape: "sb-" and 22
// base62 characters is V1, anything else V2 (sandbox.py _is_v1_sandbox_id).
// A V2 task ID is "ta-...V" (task_command_router_client.py _is_v2_task_id).
// Here a task ID is its sandbox ID's random part, so either finds the other.
const (
	idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
	v1Len   = 22
	v2Len   = 24
	// IDLen is the longest sandbox ID this makes, for the daemon's socket paths.
	IDLen = 3 + v2Len
)

func randID(prefix string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = idChars[int(b[i])%len(idChars)]
	}
	return prefix + string(b)
}

func newSandboxID(v2 bool) string {
	if v2 {
		return randID("sb-", v2Len)
	}
	return randID("sb-", v1Len)
}

func isV2(sandboxID string) bool { return len(sandboxID) != 3+v1Len }

func taskOf(sandboxID string) string {
	t := "ta-" + strings.TrimPrefix(sandboxID, "sb-")
	if isV2(sandboxID) {
		t += "V"
	}
	return t
}

func sandboxOfTask(taskID string) string {
	s := strings.TrimPrefix(taskID, "ta-")
	if len(s) == v2Len+1 {
		s = strings.TrimSuffix(s, "V")
	}
	return "sb-" + s
}

// JWTs. The client decodes a JWT's payload for its exp and nothing else; the
// router checks the signature (HS256, this process's key), the expiry, and
// that the call is for the task the token was issued for.

const jwtTTL = time.Hour

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

type claims struct {
	Task  string `json:"task,omitempty"`
	Admin bool   `json:"admin,omitempty"`
	Exp   int64  `json:"exp"`
}

func (f *Frontend) signJWT(c claims) string {
	if c.Exp == 0 {
		c.Exp = time.Now().Add(jwtTTL).Unix()
	}
	p, _ := json.Marshal(c)
	msg := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(p)
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (f *Frontend) verifyJWT(tok string) (caller, error) {
	i := strings.LastIndexByte(tok, '.')
	if i < 0 {
		return caller{}, errors.New("malformed token")
	}
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(tok[:i]))
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return caller{}, errors.New("invalid token (issued by another run of this server?)")
	}
	_, payload, _ := strings.Cut(tok[:i], ".")
	b, err := base64.RawURLEncoding.DecodeString(payload)
	var c claims
	if err != nil || json.Unmarshal(b, &c) != nil {
		return caller{}, errors.New("malformed token")
	}
	if time.Now().Unix() >= c.Exp {
		return caller{}, errors.New("token expired")
	}
	return caller{admin: c.Admin, task: c.Task}, nil
}
