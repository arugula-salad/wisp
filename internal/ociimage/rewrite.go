package ociimage

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// What the base image (images/base/Containerfile) adds on top of ubuntu, and
// the agent relies on, is small: a `sprite` account that commands run as, its
// home directory, and passwordless sudo. Everything else the agent does itself
// at boot (hostname, /etc/hosts, resolv.conf, /proc and friends, sprite-env).
// Rewrite adds the same to any image's flattened filesystem as it streams by.

// SpriteUser is the account exec sessions and services run as.
const SpriteUser = "sprite"

// ImageMetaPath is where the rewrite records the image on the disk; the agent
// reads the image's environment from it.
const ImageMetaPath = ".sprite/image.json"

// Result says what Rewrite found and added.
type Result struct {
	// Shell is the sprite user's login shell: /bin/bash, else /bin/sh, else ""
	// for an image with no shell at all (distroless).
	Shell string `json:"shell"`
	// Sudo says the image carries its own sudo, which got a NOPASSWD entry.
	// Without one the agent installs a minimal stand-in at boot.
	Sudo bool   `json:"sudo"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Home string `json:"home"`
	// UserAdded is false when the image already had a sprite account, which is then left alone.
	UserAdded bool `json:"user_added"`
}

// maxAccountFile bounds what is held in memory from /etc/passwd and friends.
const maxAccountFile = 4 << 20

type heldFile struct {
	hdr  *tar.Header
	data []byte
}

// Rewrite copies the tar stream of a flattened root filesystem (podman export)
// from in to out, adding the sprite account, its home (seeded from /etc/skel),
// a sudoers entry when the image has sudo, and meta at ImageMetaPath.
func Rewrite(in io.Reader, out io.Writer, meta []byte) (Result, error) {
	var res Result
	tr, tw := tar.NewReader(in), tar.NewWriter(out)
	// seen maps every entry to its type, and a symlink to its target, so that
	// paths can be resolved through links such as /bin -> usr/bin afterwards.
	seen := map[string]byte{}
	links := map[string]string{}
	held := map[string]*heldFile{} // account files, written at the end
	var skel []heldFile
	synth := map[string]bool{}
	var parents func(name string) error
	parents = func(name string) error {
		dir := path.Dir(name)
		if dir == "." {
			return nil
		}
		if _, ok := seen[dir]; ok {
			return nil
		}
		if err := parents(dir); err != nil {
			return err
		}
		seen[dir], synth[dir] = tar.TypeDir, true
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, ModTime: time.Now()})
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("read image filesystem: %w", err)
		}
		name := clean(hdr.Name)
		if name == "" {
			continue // the root directory itself: mkfs makes it
		}
		hdr.Name = name
		if hdr.Typeflag == tar.TypeDir {
			hdr.Name += "/"
			if synth[name] {
				// Already made up as the parent of an earlier entry; mke2fs would
				// refuse the directory twice, so this one's mode is lost.
				continue
			}
		}
		// mke2fs wants a directory before anything in it.
		if err := parents(name); err != nil {
			return res, err
		}
		seen[name] = hdr.Typeflag
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			links[name] = hdr.Linkname
		case tar.TypeLink:
			hdr.Linkname = clean(hdr.Linkname)
		}
		switch name {
		case "etc/passwd", "etc/group", "etc/shadow", "etc/gshadow":
			if hdr.Typeflag != tar.TypeReg {
				return res, fmt.Errorf("the image's /%s is not a regular file; cannot add the %s account", name, SpriteUser)
			}
			if hdr.Size > maxAccountFile {
				return res, fmt.Errorf("the image's /%s is implausibly large (%d bytes)", name, hdr.Size)
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return res, fmt.Errorf("read /%s: %w", name, err)
			}
			held[name] = &heldFile{hdr, b}
			continue
		}
		var data []byte
		if strings.HasPrefix(name, "etc/skel/") && hdr.Typeflag == tar.TypeReg && hdr.Size <= 1<<20 {
			if data, err = io.ReadAll(tr); err != nil {
				return res, fmt.Errorf("read /%s: %w", name, err)
			}
			h := *hdr
			skel = append(skel, heldFile{&h, data})
		} else if strings.HasPrefix(name, "etc/skel/") && (hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeSymlink) {
			h := *hdr
			skel = append(skel, heldFile{&h, nil})
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return res, fmt.Errorf("write /%s: %w", name, err)
		}
		if data != nil {
			_, err = tw.Write(data)
		} else if hdr.Typeflag == tar.TypeReg {
			_, err = io.Copy(tw, tr)
		}
		if err != nil {
			return res, fmt.Errorf("copy /%s: %w", name, err)
		}
	}

	fs := resolver{seen: seen, links: links}
	switch {
	case fs.exists("bin/bash") || fs.exists("usr/bin/bash"):
		res.Shell = "/bin/bash"
		if !fs.exists("bin/bash") {
			res.Shell = "/usr/bin/bash"
		}
	case fs.exists("bin/sh") || fs.exists("usr/bin/sh"):
		res.Shell = "/bin/sh"
		if !fs.exists("bin/sh") {
			res.Shell = "/usr/bin/sh"
		}
	}
	for _, p := range []string{"usr/bin/sudo", "bin/sudo", "usr/sbin/sudo", "sbin/sudo", "usr/local/bin/sudo"} {
		res.Sudo = res.Sudo || fs.exists(p)
	}

	now := time.Now()
	dir := func(name string, mode int64, uid, gid int) error {
		if _, ok := seen[strings.TrimSuffix(name, "/")]; ok {
			return nil
		}
		seen[strings.TrimSuffix(name, "/")] = tar.TypeDir
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode, Uid: uid, Gid: gid, ModTime: now})
	}
	file := func(hdr *tar.Header, name string, mode int64, data []byte) error {
		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, ModTime: now}
		if hdr != nil {
			h.Mode, h.Uid, h.Gid, h.Uname, h.Gname, h.ModTime = hdr.Mode, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname, hdr.ModTime
			h.PAXRecords = hdr.PAXRecords
		}
		h.Size = int64(len(data))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	get := func(name string) ([]byte, *tar.Header) {
		if f := held[name]; f != nil {
			return f.data, f.hdr
		}
		return nil, nil
	}

	if err := dir("etc/", 0o755, 0, 0); err != nil {
		return res, err
	}
	passwd, passwdHdr := get("etc/passwd")
	group, groupHdr := get("etc/group")
	shell := res.Shell
	if shell == "" {
		shell = "/bin/sh" // nothing to run, but a login shell field must say something
	}
	accts, err := addAccount(passwd, group, shell)
	if err != nil {
		return res, err
	}
	res.UID, res.GID, res.Home, res.UserAdded = accts.uid, accts.gid, accts.home, accts.added
	if err := file(passwdHdr, "etc/passwd", 0o644, accts.passwd); err != nil {
		return res, err
	}
	if err := file(groupHdr, "etc/group", 0o644, accts.group); err != nil {
		return res, err
	}
	// The shadow files only get a locked entry, and only where the image has them.
	for _, sf := range [][2]string{{"etc/shadow", SpriteUser + ":!::0:99999:7:::"}, {"etc/gshadow", SpriteUser + ":!::"}} {
		name, line := sf[0], sf[1]
		b, hdr := get(name)
		if hdr == nil {
			continue
		}
		if accts.added && !hasEntry(b, SpriteUser) {
			b = appendLine(b, line)
		}
		if err := file(hdr, name, 0o640, b); err != nil {
			return res, err
		}
	}
	if res.Sudo && accts.added {
		if err := dir("etc/sudoers.d/", 0o750, 0, 0); err != nil {
			return res, err
		}
		if _, ok := seen["etc/sudoers.d/"+SpriteUser]; !ok {
			if err := file(nil, "etc/sudoers.d/"+SpriteUser, 0o440, []byte(SpriteUser+" ALL=(ALL) NOPASSWD:ALL\n")); err != nil {
				return res, err
			}
		}
	}

	home := strings.Trim(res.Home, "/")
	if accts.added && home != "" && !fs.exists(home) {
		if parent := path.Dir(home); parent != "." {
			if err := dir(parent+"/", 0o755, 0, 0); err != nil {
				return res, err
			}
		}
		if err := dir(home+"/", 0o750, res.UID, res.GID); err != nil {
			return res, err
		}
		for _, f := range skel {
			h := *f.hdr
			h.Name = home + "/" + strings.TrimPrefix(h.Name, "etc/skel/")
			h.Uid, h.Gid, h.Uname, h.Gname = res.UID, res.GID, "", ""
			if h.Typeflag == tar.TypeDir {
				if err := dir(h.Name, h.Mode, res.UID, res.GID); err != nil {
					return res, err
				}
				continue
			}
			h.Size = int64(len(f.data))
			if err := tw.WriteHeader(&h); err != nil {
				return res, err
			}
			if _, err := tw.Write(f.data); err != nil {
				return res, err
			}
		}
	}

	if err := dir(".sprite/", 0o755, 0, 0); err != nil {
		return res, err
	}
	if err := file(nil, ImageMetaPath, 0o644, meta); err != nil {
		return res, err
	}
	return res, tw.Close()
}

// clean makes a tar name relative and slash-free at the ends; "" is the root.
func clean(name string) string {
	name = path.Clean("/" + name)
	return strings.TrimPrefix(name, "/")
}

// resolver answers whether a path exists in the stream, following symlinks the
// way the guest will (absolute targets are relative to the image's root).
type resolver struct {
	seen  map[string]byte
	links map[string]string
}

func (r resolver) exists(p string) bool {
	return r.resolve(p, 0) != ""
}

// resolve returns the path p really names, or "" when it does not exist.
func (r resolver) resolve(p string, depth int) string {
	if depth > 40 {
		return ""
	}
	parts := strings.Split(clean(p), "/")
	cur := ""
	for i, part := range parts {
		next := strings.TrimPrefix(cur+"/"+part, "/")
		if _, ok := r.seen[next]; !ok {
			return ""
		}
		if target, ok := r.links[next]; ok {
			if !strings.HasPrefix(target, "/") {
				target = path.Join(path.Dir("/"+next), target)
			}
			rest := strings.Join(parts[i+1:], "/")
			return r.resolve(path.Join(target, rest), depth+1)
		}
		cur = next
	}
	return cur
}

type accounts struct {
	passwd, group []byte
	uid, gid      int
	home          string
	added         bool
}

// addAccount adds the sprite user and group to the image's /etc/passwd and
// /etc/group (either may be absent: a scratch image has neither). uid and gid
// are 1000, as in the base image, unless the image already uses them (node's
// images have a `node` user there), in which case the next free ids are taken.
func addAccount(passwd, group []byte, shell string) (accounts, error) {
	a := accounts{passwd: passwd, group: group}
	users, uids := parseIDs(passwd, 2)
	groups, gids := parseIDs(group, 2)
	if f, ok := users[SpriteUser]; ok {
		// The image brought its own account; take it as it is.
		a.uid, _ = strconv.Atoi(f[2])
		a.gid, _ = strconv.Atoi(f[3])
		if len(f) > 5 {
			a.home = f[5]
		}
		return a, nil
	}
	if _, ok := users["root"]; !ok {
		a.passwd = appendLine(a.passwd, "root:x:0:0:root:/root:"+shell)
		uids[0] = true
	}
	if _, ok := groups["root"]; !ok {
		a.group = appendLine(a.group, "root:x:0:")
		gids[0] = true
	}
	a.uid = freeID(1000, uids)
	if f, ok := groups[SpriteUser]; ok {
		a.gid, _ = strconv.Atoi(f[2])
	} else {
		a.gid = a.uid
		if gids[a.gid] {
			a.gid = freeID(1000, gids)
		}
		a.group = appendLine(a.group, fmt.Sprintf("%s:x:%d:", SpriteUser, a.gid))
	}
	a.home = "/home/" + SpriteUser
	a.passwd = appendLine(a.passwd, fmt.Sprintf("%s:x:%d:%d::%s:%s", SpriteUser, a.uid, a.gid, a.home, shell))
	a.added = true
	return a, nil
}

// parseIDs indexes a passwd- or group-format file by name, and collects the
// numeric ids in field idField.
func parseIDs(b []byte, idField int) (byName map[string][]string, ids map[int]bool) {
	byName, ids = map[string][]string{}, map[int]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) <= idField || strings.HasPrefix(line, "#") {
			continue
		}
		byName[f[0]] = f
		if n, err := strconv.Atoi(f[idField]); err == nil {
			ids[n] = true
		}
	}
	return byName, ids
}

func freeID(from int, used map[int]bool) int {
	n := from
	for used[n] || n == 65534 {
		n++
	}
	return n
}

func hasEntry(b []byte, name string) bool {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, name+":") {
			return true
		}
	}
	return false
}

func appendLine(b []byte, line string) []byte {
	out := bytes.Clone(b)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, line+"\n"...)
}
