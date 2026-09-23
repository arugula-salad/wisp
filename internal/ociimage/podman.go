package ociimage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Podman runs rootless podman in the daemon's own user storage. Every call
// passes arguments as a vector, never through a shell, and every reference it
// is handed has been through ParseRef.
type Podman struct {
	Bin string // "podman" when empty
}

func (p Podman) cmd(ctx context.Context, args ...string) *exec.Cmd {
	bin := p.Bin
	if bin == "" {
		bin = "podman"
	}
	return exec.CommandContext(ctx, bin, args...)
}

func (p Podman) run(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	c := p.cmd(ctx, args...)
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("podman %s: %s", args[0], lastLine(msg))
	}
	return strings.TrimSpace(out.String()), nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Inspected is the part of `podman image inspect` a sprite disk needs.
type Inspected struct {
	ID           string   `json:"Id"`
	Digest       string   `json:"Digest"`
	RepoDigests  []string `json:"RepoDigests"`
	Architecture string   `json:"Architecture"`
	Os           string   `json:"Os"`
	Size         int64    `json:"Size"`
	Config       struct {
		Env        []string `json:"Env"`
		User       string   `json:"User"`
		WorkingDir string   `json:"WorkingDir"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"Config"`
}

// ErrNotLocal is Inspect's answer for an image podman does not hold.
var ErrNotLocal = errors.New("image not in local storage")

// Inspect reads an image from local storage by reference or ID.
func (p Podman) Inspect(ctx context.Context, image string) (Inspected, error) {
	var got []Inspected
	out, err := p.run(ctx, "image", "inspect", "--", image)
	if err != nil {
		if strings.Contains(err.Error(), "no such image") || strings.Contains(err.Error(), "image not known") {
			return Inspected{}, ErrNotLocal
		}
		return Inspected{}, err
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 {
		return Inspected{}, fmt.Errorf("podman image inspect %s: unexpected output", image)
	}
	return got[0], nil
}

// Pull fetches ref from its registry, writing podman's progress to progress.
// A localhost/ reference names an image built on this machine and is only
// looked up, never pulled. It returns the image ID.
func (p Podman) Pull(ctx context.Context, ref Ref, progress io.Writer) (string, error) {
	policy := "always"
	if ref.Local() {
		policy = "never"
	}
	var out, errb bytes.Buffer
	c := p.cmd(ctx, "pull", "--policy="+policy, "--", ref.String())
	c.Stdout = &out
	c.Stderr = &errb
	if progress != nil {
		c.Stderr = io.MultiWriter(&errb, progress)
	}
	if err := c.Run(); err != nil {
		msg := lastLine(strings.TrimSpace(errb.String()))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("pull %s: %s", ref, msg)
	}
	id := lastLine(strings.TrimSpace(out.String()))
	if id == "" {
		return "", fmt.Errorf("pull %s: podman printed no image ID", ref)
	}
	return id, nil
}

// Export streams the image's flattened root filesystem as a tar to w. podman
// exports containers, not images, so a container is created (never started)
// for the purpose and removed after.
func (p Podman) Export(ctx context.Context, id string, w io.Writer) error {
	b := make([]byte, 6)
	rand.Read(b)
	name := "wisp-export-" + hex.EncodeToString(b)
	// An image without an entrypoint or command cannot be created as is; this
	// one is never run.
	if _, err := p.run(ctx, "create", "--pull=never", "--name", name, "--entrypoint", "/.sprite-export", "--", id); err != nil {
		return err
	}
	defer p.run(context.WithoutCancel(ctx), "rm", "--force", "--", name)
	var errb bytes.Buffer
	c := p.cmd(ctx, "export", "--", name)
	c.Stdout, c.Stderr = w, &errb
	if err := c.Run(); err != nil {
		msg := lastLine(strings.TrimSpace(errb.String()))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("podman export: %s", msg)
	}
	return nil
}

// Untag drops a reference from local storage, and the image with it when it
// was the last one: what a pull made only to build a disk leaves behind.
func (p Podman) Untag(ctx context.Context, ref Ref) error {
	_, err := p.run(ctx, "rmi", "--", ref.String())
	return err
}
