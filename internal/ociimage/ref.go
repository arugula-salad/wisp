// Package ociimage turns a container image into a sprite's root filesystem:
// image references, the rewrite that makes an image's userland behave like the
// base image's (a `sprite` account, its home, sudo), and the rootless podman
// calls that fetch and flatten the image.
package ociimage

import (
	"fmt"
	"regexp"
	"strings"
)

// Image references come from API callers and end up as podman arguments, so
// they are parsed strictly and rebuilt in one canonical form, rather than
// passed through. Only registry references are accepted: podman also reads
// transports such as oci:/path or containers-storage:, which would let a caller
// name files on the host.

var (
	domainRE    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?$`)
	componentRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	tagRE       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Ref is a parsed registry reference.
type Ref struct {
	Domain string // docker.io, ghcr.io, localhost:5000, ...
	Path   string // library/node
	Tag    string // "" when only a digest is given
	Digest string // sha256:..., or ""
}

// Name is the repository, domain included: docker.io/library/node.
func (r Ref) Name() string { return r.Domain + "/" + r.Path }

// String is the canonical form: docker.io/library/node:22, with @digest when pinned.
func (r Ref) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// Local says the reference names podman's local storage, which is never pulled.
func (r Ref) Local() bool { return r.Domain == "localhost" }

// ParseRef validates and normalizes a reference the way docker does: a first
// component that is not a hostname means Docker Hub, a single component there
// means library/, and no tag and no digest means :latest.
func ParseRef(s string) (Ref, error) {
	var r Ref
	bad := func(why string) (Ref, error) { return Ref{}, fmt.Errorf("invalid image reference %q: %s", s, why) }
	if s == "" {
		return bad("empty")
	}
	if len(s) > 512 {
		return bad("too long")
	}
	if strings.ContainsAny(s, " \t\r\n\x00\\") {
		return bad("contains whitespace or control characters")
	}
	if strings.Contains(s, "://") {
		return bad("give a registry reference such as docker.io/library/alpine:3.20, not a URL")
	}
	rest := s
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		r.Digest = rest[i+1:]
		rest = rest[:i]
		if !digestRE.MatchString(r.Digest) {
			return bad("the digest must be sha256:<64 hex digits>")
		}
	}
	// A tag is a colon after the last slash; a colon before it is a registry port.
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		r.Tag = rest[i+1:]
		rest = rest[:i]
		if !tagRE.MatchString(r.Tag) {
			return bad("bad tag")
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.Domain = parts[0]
		parts = parts[1:]
		if !domainRE.MatchString(r.Domain) {
			return bad("bad registry host")
		}
	} else {
		r.Domain = "docker.io"
	}
	if r.Domain == "index.docker.io" || r.Domain == "registry-1.docker.io" {
		r.Domain = "docker.io"
	}
	for _, p := range parts {
		if !componentRE.MatchString(p) {
			return bad(fmt.Sprintf("bad path component %q (lowercase letters, digits and . _ - separators)", p))
		}
	}
	if r.Domain == "docker.io" && len(parts) == 1 {
		parts = append([]string{"library"}, parts...)
	}
	r.Path = strings.Join(parts, "/")
	if len(r.Name()) > 255 {
		return bad("repository name too long")
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}
