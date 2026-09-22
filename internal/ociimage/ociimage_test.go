package ociimage

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	good := map[string]string{
		"alpine":                                        "docker.io/library/alpine:latest",
		"node:22":                                       "docker.io/library/node:22",
		"docker.io/library/node:22":                     "docker.io/library/node:22",
		"index.docker.io/library/node:22":               "docker.io/library/node:22",
		"bitnami/redis":                                 "docker.io/bitnami/redis:latest",
		"ghcr.io/owner/repo/sub:v1.2.3":                 "ghcr.io/owner/repo/sub:v1.2.3",
		"localhost/mini-sprites-base":                   "localhost/mini-sprites-base:latest",
		"localhost:5000/team/app:dev":                   "localhost:5000/team/app:dev",
		"registry.example.com:8443/app":                 "registry.example.com:8443/app:latest",
		"gcr.io/distroless/static-debian12":             "gcr.io/distroless/static-debian12:latest",
		"alpine@sha256:" + strings.Repeat("a", 64):      "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64),
		"alpine:3.20@sha256:" + strings.Repeat("b", 64): "docker.io/library/alpine:3.20@sha256:" + strings.Repeat("b", 64),
		"python:3.12-slim-bookworm":                     "docker.io/library/python:3.12-slim-bookworm",
		"oci:foo":                                       "docker.io/library/oci:foo", // just a Hub name with a tag
		"containers-storage:alpine":                     "docker.io/library/containers-storage:alpine",
	}
	for in, want := range good {
		r, err := ParseRef(in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("ParseRef(%q) = %q, want %q", in, r, want)
		}
	}
	bad := []string{
		"", "Alpine", "alpine:", "alpine@sha256:abc", "docker://alpine", "oci:/etc/passwd",
		"oci-archive:/tmp/x.tar", "dir:/tmp", "-alpine", "--help",
		"alpine latest", "alpine;rm -rf /", "alpine\n", "a//b", "/alpine", "alpine/", "ex ample.com/a",
		"$(id)", "`id`", "alpine:" + strings.Repeat("t", 129), "host:notaport/app",
	}
	for _, in := range bad {
		if r, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) = %q, want an error", in, r)
		}
	}
	if r, _ := ParseRef("localhost/x"); !r.Local() {
		t.Error("localhost/x should be local")
	}
	if r, _ := ParseRef("localhost:5000/x"); r.Local() {
		t.Error("localhost:5000/x is a registry, not local storage")
	}
}

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
	uid              int
}

func mkTar(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Uid: e.uid}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, e.body)
	}
	tw.Close()
	return buf.Bytes()
}

type fsView map[string]*tar.Header

func readTar(t *testing.T, b []byte) (fsView, map[string]string, []string) {
	t.Helper()
	hdrs, bodies := fsView{}, map[string]string{}
	var order []string
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(h.Name, "/")
		if _, dup := hdrs[name]; dup {
			t.Errorf("%s appears twice", name)
		}
		body, _ := io.ReadAll(tr)
		hdrs[name], bodies[name] = h, string(body)
		order = append(order, name)
	}
	return hdrs, bodies, order
}

func rewrite(t *testing.T, entries []entry) (Result, fsView, map[string]string) {
	t.Helper()
	var out bytes.Buffer
	res, err := Rewrite(bytes.NewReader(mkTar(t, entries)), &out, []byte(`{"env":["A=1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	h, b, _ := readTar(t, out.Bytes())
	return res, h, b
}

func TestRewriteAlpineLike(t *testing.T) {
	res, h, b := rewrite(t, []entry{
		{name: "bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "bin/busybox", body: "ELF", mode: 0o755},
		{name: "bin/sh", typ: tar.TypeSymlink, link: "/bin/busybox"},
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", body: "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin"},
		{name: "etc/group", body: "root:x:0:root\n"},
		{name: "etc/shadow", body: "root:*::0:::::\n", mode: 0o640},
		{name: "etc/skel/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/skel/.profile", body: "PS1=x\n"},
		{name: "home/", typ: tar.TypeDir, mode: 0o755},
	})
	if res.Shell != "/bin/sh" || res.Sudo || res.UID != 1000 || res.GID != 1000 || !res.UserAdded {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasSuffix(b["etc/passwd"], "\nsprite:x:1000:1000::/home/sprite:/bin/sh\n") ||
		!strings.Contains(b["etc/passwd"], "/sbin/nologin\n") {
		t.Errorf("passwd = %q", b["etc/passwd"])
	}
	if !strings.HasSuffix(b["etc/group"], "root:x:0:root\nsprite:x:1000:\n") {
		t.Errorf("group = %q", b["etc/group"])
	}
	if !strings.Contains(b["etc/shadow"], "sprite:!:") || h["etc/shadow"].Mode != 0o640 {
		t.Errorf("shadow = %q mode %o", b["etc/shadow"], h["etc/shadow"].Mode)
	}
	if home := h["home/sprite"]; home == nil || home.Uid != 1000 || home.Gid != 1000 || home.Typeflag != tar.TypeDir {
		t.Errorf("home = %+v", home)
	}
	if p := h["home/sprite/.profile"]; p == nil || p.Uid != 1000 || b["home/sprite/.profile"] != "PS1=x\n" {
		t.Errorf("skel copy = %+v", p)
	}
	if b[ImageMetaPath] != `{"env":["A=1"]}` {
		t.Errorf("meta = %q", b[ImageMetaPath])
	}
	if _, ok := h["etc/sudoers.d/sprite"]; ok {
		t.Error("sudoers entry without sudo")
	}
	if h["bin/busybox"].Mode != 0o755 || h["bin/sh"].Linkname != "/bin/busybox" {
		t.Error("entries must pass through unchanged")
	}
}

func TestRewriteDebianLikeWithUID1000Taken(t *testing.T) {
	res, h, b := rewrite(t, []entry{
		{name: "bin", typ: tar.TypeSymlink, link: "usr/bin"},
		{name: "usr/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "usr/bin/bash", body: "ELF", mode: 0o755},
		{name: "usr/bin/sudo", body: "ELF", mode: 0o4755},
		{name: "usr/bin/sh", typ: tar.TypeSymlink, link: "dash"},
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", body: "root:x:0:0:root:/root:/bin/bash\nnode:x:1000:1000::/home/node:/bin/bash\n"},
		{name: "etc/group", body: "root:x:0:\nnode:x:1000:\n"},
		{name: "etc/sudoers.d/", typ: tar.TypeDir, mode: 0o750},
		{name: "home/", typ: tar.TypeDir, mode: 0o755},
		{name: "home/node/", typ: tar.TypeDir, mode: 0o755, uid: 1000},
	})
	// bin/bash resolves through the bin -> usr/bin link.
	if res.Shell != "/bin/bash" || !res.Sudo || res.UID != 1001 || res.GID != 1001 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.HasSuffix(b["etc/passwd"], "sprite:x:1001:1001::/home/sprite:/bin/bash\n") {
		t.Errorf("passwd = %q", b["etc/passwd"])
	}
	if sd := h["etc/sudoers.d/sprite"]; sd == nil || sd.Mode != 0o440 || b["etc/sudoers.d/sprite"] != "sprite ALL=(ALL) NOPASSWD:ALL\n" {
		t.Errorf("sudoers = %+v %q", sd, b["etc/sudoers.d/sprite"])
	}
	if h["home/node"].Uid != 1000 || h["home/sprite"].Uid != 1001 {
		t.Error("home dirs")
	}
}

func TestRewriteScratchImage(t *testing.T) {
	// No /etc at all, no shell: a static binary and nothing else.
	res, h, b := rewrite(t, []entry{{name: "app", body: "ELF", mode: 0o755}})
	if res.Shell != "" || res.UID != 1000 {
		t.Fatalf("result = %+v", res)
	}
	if h["etc"] == nil || b["etc/passwd"] != "root:x:0:0:root:/root:/bin/sh\nsprite:x:1000:1000::/home/sprite:/bin/sh\n" {
		t.Errorf("passwd = %q", b["etc/passwd"])
	}
	if b["etc/group"] != "root:x:0:\nsprite:x:1000:\n" {
		t.Errorf("group = %q", b["etc/group"])
	}
	if h["home"] == nil || h["home/sprite"] == nil || h[".sprite"] == nil {
		t.Error("missing directories")
	}
}

func TestRewriteKeepsAnExistingSpriteUser(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/sh\nsprite:x:1234:1234::/srv/sprite:/bin/sh\n"
	res, h, b := rewrite(t, []entry{
		{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		{name: "etc/passwd", body: passwd},
	})
	if res.UserAdded || res.UID != 1234 || res.Home != "/srv/sprite" || b["etc/passwd"] != passwd {
		t.Fatalf("result = %+v, passwd %q", res, b["etc/passwd"])
	}
	if _, ok := h["srv/sprite"]; ok {
		t.Error("an image's own account is left alone")
	}
}

func TestRewriteRefusesLinkedPasswd(t *testing.T) {
	in := mkTar(t, []entry{{name: "etc/passwd", typ: tar.TypeSymlink, link: "/usr/share/passwd"}})
	if _, err := Rewrite(bytes.NewReader(in), io.Discard, nil); err == nil {
		t.Fatal("want an error for a symlinked /etc/passwd")
	}
}

func TestResolverFollowsLinks(t *testing.T) {
	r := resolver{
		seen:  map[string]byte{"bin": tar.TypeSymlink, "usr": tar.TypeDir, "usr/bin": tar.TypeDir, "usr/bin/sh": tar.TypeSymlink, "usr/bin/dash": tar.TypeReg, "loop": tar.TypeSymlink},
		links: map[string]string{"bin": "/usr/bin", "usr/bin/sh": "dash", "loop": "loop"},
	}
	for p, want := range map[string]bool{"bin/sh": true, "bin/dash": true, "usr/bin/sh": true, "bin/bash": false, "loop/x": false} {
		if got := r.exists(p); got != want {
			t.Errorf("exists(%s) = %v", p, got)
		}
	}
}

func TestRewriteMakesUpMissingParents(t *testing.T) {
	var out bytes.Buffer
	in := mkTar(t, []entry{
		{name: "usr/lib/x/file", body: "x"},
		{name: "usr/", typ: tar.TypeDir, mode: 0o700}, // late: dropped, the made-up one stands
		{name: "usr/lib/y", body: "y"},
	})
	if _, err := Rewrite(bytes.NewReader(in), &out, nil); err != nil {
		t.Fatal(err)
	}
	h, _, order := readTar(t, out.Bytes())
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	for _, n := range []string{"usr", "usr/lib", "usr/lib/x"} {
		if h[n] == nil || h[n].Typeflag != tar.TypeDir || pos[n] > pos["usr/lib/x/file"] {
			t.Errorf("%s: %+v at %d", n, h[n], pos[n])
		}
	}
}
