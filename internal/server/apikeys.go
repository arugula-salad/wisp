package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// API keys: named bearer tokens beside the root token in <data>/token, each
// revocable on its own and scoped admin (the whole API, like the root token)
// or read (look, never touch). Only a hash of each key is kept, in
// <data>/keys.json; the key itself is shown once, when it is made. Keys are
// managed over the operator socket (wispd keys) and from the dashboard, never
// through the bearer API, so a leaked key cannot mint another.
//
// A key reads wisp_<id>_<secret>: the id finds the record without trying every
// hash, and is what lists and logs show.

const (
	ScopeAdmin = "admin"
	ScopeRead  = "read"

	keysFile   = "keys.json"
	keyPrefix  = "wisp_"
	rootKeyID  = "root"
	keyNameMax = 64
	// lastUsedEvery bounds how often a busy key's last_used_at is written out.
	lastUsedEvery = time.Minute
)

// APIKey is a key's record: everything but the key.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Hash       string     `json:"hash,omitempty"`
}

// NewAPIKey is the answer to making one, the only time the key is seen.
type NewAPIKey struct {
	APIKey
	Key string `json:"key"`
}

// principal is who a request authenticated as.
type principal struct {
	ID, Name, Scope string
}

func (p principal) admin() bool { return p.Scope == ScopeAdmin }

type keyring struct {
	path string
	// broken is why keys.json could not be read. Then no key works and none can
	// be made or revoked, which would overwrite the file: the root token still does.
	broken error
	mu     sync.Mutex
	keys   []APIKey
}

func openKeyring(dataDir string) *keyring {
	k := &keyring{path: filepath.Join(dataDir, keysFile)}
	b, err := os.ReadFile(k.path)
	if errors.Is(err, os.ErrNotExist) {
		return k
	}
	if err == nil {
		err = json.Unmarshal(b, &k.keys)
	}
	if err != nil {
		k.broken, k.keys = fmt.Errorf("%s: %w", k.path, err), nil
	}
	return k
}

// save writes the file whole; the caller holds mu.
func (k *keyring) save() error {
	b, err := json.MarshalIndent(k.keys, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.path)
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func validScope(s string) bool { return s == ScopeAdmin || s == ScopeRead }

func (k *keyring) create(name, scope string) (NewAPIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > keyNameMax || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' }) {
		return NewAPIKey{}, fmt.Errorf("a key needs a name of 1-%d printable characters", keyNameMax)
	}
	if !validScope(scope) {
		return NewAPIKey{}, fmt.Errorf("scope is %q or %q, not %q", ScopeAdmin, ScopeRead, scope)
	}
	if k.broken != nil {
		return NewAPIKey{}, k.broken
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if slices.ContainsFunc(k.keys, func(a APIKey) bool { return a.Name == name }) {
		return NewAPIKey{}, fmt.Errorf("there is already a key named %q", name)
	}
	id, secret := make([]byte, 4), make([]byte, 32)
	rand.Read(id)
	rand.Read(secret)
	rec := APIKey{ID: hex.EncodeToString(id), Name: name, Scope: scope, CreatedAt: time.Now().UTC()}
	key := keyPrefix + rec.ID + "_" + hex.EncodeToString(secret)
	rec.Hash = hashKey(key)
	k.keys = append(k.keys, rec)
	if err := k.save(); err != nil {
		k.keys = k.keys[:len(k.keys)-1]
		return NewAPIKey{}, err
	}
	rec.Hash = ""
	return NewAPIKey{APIKey: rec, Key: key}, nil
}

// list is every key, without hashes, oldest first.
func (k *keyring) list() []APIKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]APIKey, len(k.keys))
	for i, a := range k.keys {
		a.Hash = ""
		out[i] = a
	}
	return out
}

var errNoSuchKey = errors.New("no such key")

// revoke deletes a key by id or name. Requests carrying it fail from now on,
// dashboard sessions signed in with it included.
func (k *keyring) revoke(idOrName string) (APIKey, error) {
	if k.broken != nil {
		return APIKey{}, k.broken
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.keys, func(a APIKey) bool { return a.ID == idOrName || a.Name == idOrName })
	if i < 0 {
		return APIKey{}, errNoSuchKey
	}
	gone := k.keys[i]
	k.keys = slices.Delete(k.keys, i, i+1)
	if err := k.save(); err != nil {
		k.keys = slices.Insert(k.keys, i, gone)
		return APIKey{}, err
	}
	gone.Hash = ""
	return gone, nil
}

// get finds a live key by id.
func (k *keyring) get(id string) (APIKey, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.keys, func(a APIKey) bool { return a.ID == id })
	if i < 0 {
		return APIKey{}, false
	}
	return k.keys[i], true
}

// check authenticates a presented key and notes its use.
func (k *keyring) check(key string) (principal, bool) {
	rest, ok := strings.CutPrefix(key, keyPrefix)
	if !ok {
		return principal{}, false
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok {
		return principal{}, false
	}
	want := hashKey(key)
	k.mu.Lock()
	defer k.mu.Unlock()
	i := slices.IndexFunc(k.keys, func(a APIKey) bool { return a.ID == id })
	if i < 0 || subtle.ConstantTimeCompare([]byte(want), []byte(k.keys[i].Hash)) != 1 {
		return principal{}, false
	}
	a := &k.keys[i]
	if now := time.Now().UTC(); a.LastUsedAt == nil || now.Sub(*a.LastUsedAt) >= lastUsedEvery {
		a.LastUsedAt = &now
		k.save() // best effort: a stale last_used_at is no reason to refuse the request
	}
	return principal{ID: a.ID, Name: a.Name, Scope: a.Scope}, true
}

// authenticate resolves a bearer token: the root token, or a key.
func (s *Server) authenticate(tok string) (principal, bool) {
	if tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1 {
		return principal{ID: rootKeyID, Name: "root token", Scope: ScopeAdmin}, true
	}
	if s.keys == nil {
		return principal{}, false
	}
	return s.keys.check(tok)
}

// principalByID is who a dashboard session belongs to, if that key still exists.
func (s *Server) principalByID(id string) (principal, bool) {
	if id == rootKeyID {
		return principal{ID: rootKeyID, Name: "root token", Scope: ScopeAdmin}, true
	}
	if s.keys == nil {
		return principal{}, false
	}
	a, ok := s.keys.get(id)
	return principal{ID: a.ID, Name: a.Name, Scope: a.Scope}, ok
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// readOnly says whether a read-scoped key may make this request: GET and HEAD
// only, and no WebSockets, which are how exec, the TCP proxy and /control
// reach inside a sprite. What a read key can see still includes a sprite's
// files (GET .../fs/read); it cannot change anything.
func readOnly(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && !websocket.IsWebSocketUpgrade(r)
}

// registerKeyOps puts key management on mux, for the operator socket (whoever
// can open it can read the root token anyway) and the dashboard.
func (s *Server) registerKeyOps(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.keys.list())
	})
	mux.HandleFunc("POST "+prefix+"/keys", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name  string `json:"name"`
			Scope string `json:"scope"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "body is {\"name\": ..., \"scope\": \"admin\"|\"read\"}")
			return
		}
		if req.Scope == "" {
			req.Scope = ScopeAdmin
		}
		k, err := s.keys.create(req.Name, req.Scope)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		s.log.Info("API key created", "id", k.ID, "name", k.Name, "scope", k.Scope, "via", viaOf(r))
		writeJSON(w, http.StatusCreated, k)
	})
	mux.HandleFunc("DELETE "+prefix+"/keys/{key}", func(w http.ResponseWriter, r *http.Request) {
		k, err := s.keys.revoke(r.PathValue("key"))
		if errors.Is(err, errNoSuchKey) {
			writeErr(w, http.StatusNotFound, "not_found", "no key with that id or name")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "revoke_failed", err.Error())
			return
		}
		s.log.Info("API key revoked", "id", k.ID, "name", k.Name, "via", viaOf(r))
		writeJSON(w, http.StatusOK, k)
	})
}

func viaOf(r *http.Request) string {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return "dashboard (" + p.Name + ")"
	}
	return "operator socket"
}

type principalKey struct{}
