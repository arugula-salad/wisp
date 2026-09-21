package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fsDo(t *testing.T, method, u string, body io.Reader) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, u, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestFilesystemAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	root := t.TempDir()
	q := func(path string, extra ...string) string {
		v := url.Values{"path": {path}, "workingDir": {root}}
		for i := 0; i+1 < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return "?" + v.Encode()
	}

	if code, b := fsDo(t, "PUT", ts.URL+"/fs/write"+q("a/b/f.txt", "mode", "0600"), strings.NewReader("x")); code != http.StatusNotFound {
		t.Fatalf("write without mkdirParents into a missing dir: %d %s", code, b)
	}
	if code, b := fsDo(t, "PUT", ts.URL+"/fs/write"+q("a/b/f.txt", "mode", "0600", "mkdirParents", "true"), strings.NewReader("hello")); code != http.StatusOK {
		t.Fatalf("write: %d %s", code, b)
	}
	if st, err := os.Stat(filepath.Join(root, "a/b/f.txt")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("on disk: %v %v", st, err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "a/b/.sprite-write-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	if code, b := fsDo(t, "GET", ts.URL+"/fs/read"+q("a/b/f.txt"), nil); code != 200 || string(b) != "hello" {
		t.Fatalf("read: %d %q", code, b)
	}
	if code, _ := fsDo(t, "GET", ts.URL+"/fs/read"+q("a/b"), nil); code != http.StatusBadRequest {
		t.Fatalf("read of a directory: %d, want 400", code)
	}

	var list struct {
		Entries []fsEntry
		Count   int
	}
	_, b := fsDo(t, "GET", ts.URL+"/fs/list"+q("a/b/f.txt"), nil)
	json.Unmarshal(b, &list)
	if list.Count != 1 || list.Entries[0].Name != "f.txt" || list.Entries[0].Mode != "0600" || list.Entries[0].Size != 5 || list.Entries[0].Type != "file" {
		t.Fatalf("list of a file (this is how clients stat): %s", b)
	}

	body, _ := json.Marshal(map[string]any{"source": "a", "dest": "copy", "workingDir": root, "recursive": true})
	if code, b := fsDo(t, "POST", ts.URL+"/fs/copy", bytes.NewReader(body)); code != 200 {
		t.Fatalf("copy: %d %s", code, b)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "copy/b/f.txt")); string(got) != "hello" {
		t.Fatalf("copied content %q", got)
	}
	body, _ = json.Marshal(map[string]any{"path": "copy", "workingDir": root, "mode": "0700", "recursive": true})
	fsDo(t, "POST", ts.URL+"/fs/chmod", bytes.NewReader(body))
	if st, _ := os.Stat(filepath.Join(root, "copy/b/f.txt")); st.Mode().Perm() != 0o700 {
		t.Fatalf("recursive chmod: %v", st.Mode())
	}

	if code, _ := fsDo(t, "DELETE", ts.URL+"/fs/delete"+q("a"), nil); code != http.StatusConflict {
		t.Fatalf("non-recursive delete of non-empty dir: %d, want 409", code)
	}
	if code, _ := fsDo(t, "DELETE", ts.URL+"/fs/delete"+q("a", "recursive", "true"), nil); code != 200 {
		t.Fatalf("recursive delete: %d", code)
	}
	if code, b := fsDo(t, "DELETE", ts.URL+"/fs/delete"+q("a"), nil); code != 404 || !strings.Contains(string(b), `"code":"not_found"`) {
		t.Fatalf("delete missing: %d %s", code, b)
	}
	if code, _ := fsDo(t, "DELETE", ts.URL+"/fs/delete?path=/&recursive=true", nil); code != http.StatusBadRequest {
		t.Fatalf("delete of / must be refused, got %d", code)
	}
}
