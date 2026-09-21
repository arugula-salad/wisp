package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseInterleavesFlagsAndPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cmd := fs.String("cmd", "", "")
	args := fs.String("args", "", "")
	port := fs.Int("http-port", 0, "")
	pos, err := parse(fs, []string{"web", "--cmd", "python3", "--args", "-m,http.server,3000", "--http-port", "3000"}, 1)
	if err != nil || !reflect.DeepEqual(pos, []string{"web"}) || *cmd != "python3" || *args != "-m,http.server,3000" || *port != 3000 {
		t.Fatalf("pos=%v cmd=%q args=%q port=%d err=%v", pos, *cmd, *args, *port, err)
	}
	if _, err := parse(flag.NewFlagSet("t", flag.ContinueOnError), []string{"a", "b"}, 1); err == nil {
		t.Error("extra positional accepted")
	}
}

// serve answers on a unix socket the way the agent does and records the request.
func serve(t *testing.T, status int, contentType, response string) *http.Request {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPRITE_ENV_SOCKET", sock)
	got := &http.Request{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r
		b, _ := io.ReadAll(r.Body)
		got.Body = io.NopCloser(bytes.NewReader(b))
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, response)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return got
}

func TestServicesCreateRequest(t *testing.T) {
	got := serve(t, 200, "application/x-ndjson", `{"type":"started"}`+"\n")
	err := services("create", []string{"web", "--cmd", "python3", "--args", "-m,http.server,3000",
		"--env", "PORT=3000,DEBUG=1", "--needs", "db", "--http-port", "3000", "--duration", "1s", "--dir", "/srv"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPut || got.URL.Path != "/v1/services/web" || got.URL.Query().Get("duration") != "1s" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	var def map[string]any
	json.NewDecoder(got.Body).Decode(&def)
	want := map[string]any{"cmd": "python3", "args": []any{"-m", "http.server", "3000"}, "needs": []any{"db"},
		"http_port": 3000.0, "dir": "/srv", "env": map[string]any{"PORT": "3000", "DEBUG": "1"}}
	if !reflect.DeepEqual(def, want) {
		t.Errorf("definition = %v\nwant %v", def, want)
	}
}

func TestSpritesCreateRequest(t *testing.T) {
	got := serve(t, 201, "application/json", `{"name":"game-1"}`)
	if err := sprites("create", []string{"game-1", "--from", "template@v2", "--public", "--env", "SEED=7"}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v1/sprites" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	var body map[string]any
	json.NewDecoder(got.Body).Decode(&body)
	want := map[string]any{"name": "game-1", "from": map[string]any{"sprite": "template", "checkpoint": "v2"},
		"url_settings": map[string]any{"auth": "public"}, "environment": map[string]any{"SEED": "7"}}
	if !reflect.DeepEqual(body, want) {
		t.Errorf("body = %v\nwant %v", body, want)
	}
}

func TestFailuresBecomeErrors(t *testing.T) {
	serve(t, 409, "application/json", `{"error":"conflict","message":"another service already has an HTTP port configured"}`)
	if err := services("create", []string{"web", "--cmd", "x"}); err == nil || err.Error() != "another service already has an HTTP port configured (409)" {
		t.Errorf("HTTP error: %v", err)
	}
	serve(t, 200, "application/x-ndjson", `{"type":"info","data":"Creating..."}`+"\n"+`{"type":"error","error":"clone disk: no space"}`+"\n")
	if err := checkpoints("create", nil); err == nil || err.Error() != "clone disk: no space" {
		t.Errorf("stream error: %v", err)
	}
}
