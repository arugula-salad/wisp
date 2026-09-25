package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// fromInside sends a request down the named sprite's guest channel.
func fromInside(t *testing.T, s *Server, sprite, method, path, body string) *http.Response {
	t.Helper()
	sp, err := s.store.Get(sprite)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.guestAPI(sp, &guestChan{}).ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Result()
}

func status(t *testing.T, resp *http.Response, want int) []byte {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, body)
	}
	return body
}

func names(t *testing.T, body []byte) []string {
	t.Helper()
	var list struct {
		Sprites []struct{ Name string } `json:"sprites"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, sp := range list.Sprites {
		out = append(out, sp.Name)
	}
	return out
}

func TestASpriteManagesOnlyTheSpritesItCreated(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	for _, name := range []string{"lobby", "other-lobby", "bystander"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`"}`), http.StatusCreated)
	}

	// Off by default, and the refusal covers every route.
	for _, req := range [][2]string{{"POST", "/v1/sprites"}, {"GET", "/v1/sprites"}, {"GET", "/v1/sprites/bystander"}, {"DELETE", "/v1/sprites/bystander"}} {
		if body := status(t, fromInside(t, s, "lobby", req[0], req[1], `{"name":"x"}`), http.StatusForbidden); !strings.Contains(string(body), "spawn_disabled") {
			t.Errorf("%s %s: %s", req[0], req[1], body)
		}
	}
	for _, name := range []string{"lobby", "other-lobby"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites/"+name+"/policy/spawn", `{"enabled":true,"max_children":2}`), http.StatusNoContent)
	}

	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-1","url_settings":{"auth":"public"}}`), http.StatusCreated)
	status(t, fromInside(t, s, "other-lobby", "POST", "/v1/sprites", `{"name":"theirs"}`), http.StatusCreated)
	lobby, _ := s.store.Get("lobby")
	game, _ := s.store.Get("game-1")
	if game.ParentID != lobby.ID || game.URLSettings.Auth != "public" || game.Spawn != nil {
		t.Fatalf("child = %+v", game)
	}
	if _, err := os.Stat(filepath.Join(s.store.Dir(game.ID), vmm.DiskFile)); err != nil {
		t.Fatalf("the child has no disk: %v", err)
	}

	if got := names(t, status(t, fromInside(t, s, "lobby", "GET", "/v1/sprites", ""), http.StatusOK)); len(got) != 1 || got[0] != "game-1" {
		t.Errorf("list from inside = %v, want only its child", got)
	}
	for _, name := range []string{"bystander", "theirs", "lobby", "missing"} {
		status(t, fromInside(t, s, "lobby", "GET", "/v1/sprites/"+name, ""), http.StatusNotFound)
		status(t, fromInside(t, s, "lobby", "DELETE", "/v1/sprites/"+name, ""), http.StatusNotFound)
		if _, err := s.store.Get(name); name != "missing" && err != nil {
			t.Fatalf("%s was deleted by a sprite that did not create it", name)
		}
	}

	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-2"}`), http.StatusCreated)
	e := apiError(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-3"}`))
	if e.StatusCode != http.StatusForbidden || e.ErrorCode != codeSpriteLimit || e.Limit != 2 || e.CurrentCount != 2 {
		t.Fatalf("over max_children: %+v", e)
	}
	status(t, fromInside(t, s, "lobby", "DELETE", "/v1/sprites/game-1", ""), http.StatusNoContent)
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game-3"}`), http.StatusCreated)

	// Revoking takes effect on the open channel.
	status(t, apiCall(t, h, "DELETE", "/v1/sprites/lobby/policy/spawn", ""), http.StatusNoContent)
	status(t, fromInside(t, s, "lobby", "GET", "/v1/sprites", ""), http.StatusForbidden)
}

func TestAChildCannotEscapeItsParentsPolicies(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lobby","config":{"ram_mb":512}}`), http.StatusCreated)
	rules := []store.NetworkRule{{Domain: "github.com", Action: "allow"}}
	s.store.Update("lobby", func(sp *store.Sprite) {
		sp.NetworkRules = rules
		sp.Spawn = &store.SpawnPolicy{Enabled: true}
		sp.Privileges = &store.PrivilegesPolicy{Profile: "minimal"}
	})
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game","config":{"ram_mb":65536}}`), http.StatusCreated)
	game, _ := s.store.Get("game")
	if len(game.NetworkRules) != 1 || game.NetworkRules[0] != rules[0] {
		t.Errorf("network rules = %v, want the parent's", game.NetworkRules)
	}
	if game.Config.RamMB != 512 || game.Privileges == nil || game.Privileges.Profile != "minimal" {
		t.Errorf("config = %+v privileges = %+v, want the parent's", game.Config, game.Privileges)
	}
}

func TestCloningFromACheckpoint(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"template","config":{"ram_mb":768}}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lobby"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"private"}`), http.StatusCreated)
	tmpl, _ := s.store.Get("template")
	setDisk := func(name, v string) {
		sp, _ := s.store.Get(name)
		if err := os.WriteFile(filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	disk := func(name string) string {
		sp, _ := s.store.Get(name)
		b, _ := os.ReadFile(filepath.Join(s.store.Dir(sp.ID), vmm.DiskFile))
		return string(b)
	}

	e := apiError(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"early","from":{"sprite":"template"}}`))
	if e.StatusCode != http.StatusNotFound || e.ErrorCode != "checkpoint_not_found" {
		t.Fatalf("no checkpoint yet: %+v", e)
	}
	if _, err := s.store.Get("early"); err == nil {
		t.Fatal("a failed clone left a sprite behind")
	}

	// No body is fine (above, and below); a body that is not JSON is not.
	status(t, apiCall(t, h, "POST", "/v1/sprites/template/checkpoint", "{not json"), http.StatusBadRequest)
	for _, name := range []string{"template", "private"} {
		setDisk(name, name+" with the game installed")
		status(t, apiCall(t, h, "POST", "/v1/sprites/"+name+"/checkpoint", ""), http.StatusOK)
	}
	setDisk("template", "template, changed since")

	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"copy","from":{"sprite":"template"}}`), http.StatusCreated)
	if got := disk("copy"); got != "template with the game installed" {
		t.Errorf("clone disk = %q, want the checkpoint's", got)
	}
	if copy, _ := s.store.Get("copy"); copy.Config.RamMB != 768 || copy.ID == tmpl.ID || len(copy.Checkpoints) != 0 {
		t.Errorf("clone = %+v", copy)
	}
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"nope","from":{"sprite":"template","checkpoint":"v9"}}`), http.StatusNotFound)

	// From inside: only sources the operator listed, its own and its children's.
	status(t, apiCall(t, h, "POST", "/v1/sprites/lobby/policy/spawn", `{"enabled":true,"sources":["template"]}`), http.StatusNoContent)
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game","from":{"sprite":"template","checkpoint":"v1"}}`), http.StatusCreated)
	if got := disk("game"); got != "template with the game installed" {
		t.Errorf("game disk = %q", got)
	}
	if game, _ := s.store.Get("game"); game.Config.RamMB != 768 {
		t.Errorf("a clone keeps its source's machine shape, got %+v", game.Config)
	}
	e = apiError(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"stolen","from":{"sprite":"private"}}`))
	if e.StatusCode != http.StatusNotFound || e.ErrorCode != "source_not_found" {
		t.Fatalf("cloning a sprite it has no business with: %+v", e)
	}
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"self","from":{}}`), http.StatusNotFound) // lobby has no checkpoint
}
