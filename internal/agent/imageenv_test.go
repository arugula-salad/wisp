package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestImageEnv(t *testing.T) {
	dir := t.TempDir()
	old := imageMetaPath
	imageMetaPath = filepath.Join(dir, "image.json")
	t.Cleanup(func() { imageMetaPath = old })

	if env := imageEnv(); env != nil {
		t.Fatalf("no file: %q", env)
	}
	os.WriteFile(imageMetaPath, []byte(`{"env":["PATH=/usr/local/go/bin:/usr/bin","HOME=/root","GOPATH=/go","junk"]}`), 0o644)
	env := imageEnv()
	want := []string{"PATH=/usr/local/go/bin:/usr/bin:/usr/local/bin", "GOPATH=/go"}
	if !slices.Equal(env, want) {
		t.Fatalf("imageEnv = %q, want %q", env, want)
	}
	base := baseEnv("/home/sprite", "sprite")
	if "PATH="+envValue(base, "PATH") != want[0] || envValue(base, "HOME") != "/home/sprite" || envValue(base, "GOPATH") != "/go" {
		t.Fatalf("baseEnv = %q", base)
	}

	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "tool"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "data"), nil, 0o644)
	if p, err := lookPath("tool", []string{"PATH=/nonexistent:" + bin}); err != nil || p != filepath.Join(bin, "tool") {
		t.Errorf("lookPath tool = %q, %v", p, err)
	}
	if _, err := lookPath("data", []string{"PATH=" + bin}); err == nil {
		t.Error("a file that is not executable is not a command")
	}
}
