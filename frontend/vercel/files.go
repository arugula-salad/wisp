package vercel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/arugula-salad/wisp/internal/store"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// Files: fs/write takes a gzip-compressed tar and extracts it under x-cwd;
// fs/read answers a file's bytes; fs/mkdir makes a directory. Everything else
// the SDKs' filesystem helpers do (stat, ls, rm, ...) they do with commands.

// resolve is p made absolute against cwd, else the default user's home.
func (f *Frontend) resolve(p, cwd string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	if cwd == "" {
		cwd = f.opts.Home
	} else if !path.IsAbs(cwd) {
		cwd = path.Join(f.opts.Home, cwd)
	}
	return path.Join(cwd, p)
}

// fileErr answers a filesystem failure as hosted does: 400 file_error.
func fileErr(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, "file_error", msg)
}

// writeFiles is POST …/fs/write.
func (f *Frontend) writeFiles(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	base := r.Header.Get("x-cwd")
	if base == "" {
		base = "/"
	}
	base = f.resolve(base, "")
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		stopped(w)
		return
	}
	defer release()
	if err := f.extract(r.Context(), mach, base, http.MaxBytesReader(w, r.Body, 4<<30)); err != nil {
		var be badArchive
		var fe agentFSError
		switch {
		case errors.As(err, &be):
			writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: "+be.Error())
		case errors.As(err, &fe):
			fileErr(w, "error writing file: "+fe.msg)
		default:
			fileErr(w, "error writing files: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// badArchive is a body that is not a gzip-compressed tar.
type badArchive struct{ err error }

func (e badArchive) Error() string { return "the body must be a gzip-compressed tar: " + e.err.Error() }

// extract writes a gzip-compressed tar's entries under base: regular files
// through the agent's filesystem API (with their modes, parents created),
// directories and symlinks with one command afterwards. Entry names are
// relative to base (the SDKs make them so); one that climbs out of base with
// .. stays inside it, as tar's own extraction keeps it.
func (f *Frontend) extract(ctx context.Context, m *vmm.Machine, base string, body io.Reader) error {
	zr, err := gzip.NewReader(body)
	if err != nil {
		return badArchive{err}
	}
	tr := tar.NewReader(zr)
	var dirs []string
	var links [][2]string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return badArchive{err}
		}
		name := path.Join(base, path.Clean("/"+hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			// Limited: net/http reads past ContentLength to catch an overlong
			// body, maybe after Do returns, and must not eat the next entry.
			if err := f.writeFile(ctx, m, name, hdr.Mode, hdr.Size, io.LimitReader(tr, hdr.Size)); err != nil {
				return err
			}
		case tar.TypeDir:
			dirs = append(dirs, name)
		case tar.TypeSymlink:
			links = append(links, [2]string{hdr.Linkname, name})
		case tar.TypeLink:
			links = append(links, [2]string{path.Join(base, path.Clean("/"+hdr.Linkname)), name})
		}
	}
	if len(dirs) == 0 && len(links) == 0 {
		return nil
	}
	// One command for the rest, as the default user: mkdir -p for the
	// directories, ln -sfn for the links.
	script := `set -e
n=$1; shift
i=0; while [ $i -lt $n ]; do mkdir -p -- "$1"; shift; i=$((i+1)); done
while [ $# -gt 0 ]; do mkdir -p -- "$(dirname -- "$2")"; ln -sfn -- "$1" "$2"; shift 2; done`
	argv := []string{"sh", "-c", script, "vercel-fs-write", strconv.Itoa(len(dirs))}
	argv = append(argv, dirs...)
	for _, l := range links {
		argv = append(argv, l[0], l[1])
	}
	_, stderr, code, err := f.runShort(ctx, m, execSpec{argv: argv, dir: "/"})
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New(strings.TrimSpace(stderr))
	}
	return nil
}

// readFile is POST …/fs/read: the file's bytes as application/octet-stream
// (which the JS SDK requires), or 404 not_found.
func (f *Frontend) readFile(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var req struct {
		Path string `json:"path"`
		Cwd  string `json:"cwd"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `path` is required.")
		return
	}
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		stopped(w)
		return
	}
	defer release()
	resp, err := f.openFile(r.Context(), mach, f.resolve(req.Path, req.Cwd))
	if err != nil {
		var fe agentFSError
		if errors.As(err, &fe) && fe.status == http.StatusNotFound {
			writeErr(w, http.StatusNotFound, "not_found", "File not found.")
			return
		}
		if errors.As(err, &fe) {
			fileErr(w, "error reading file: "+fe.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Failed to read file: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(http.StatusOK)
	io.Copy(w, resp.Body)
}

// mkdir is POST …/fs/mkdir: one directory, or with recursive its parents
// too, made by the default user. Hosted's errors: 400 file_error "error
// creating directory: <path>: File exists", or "...: No such file or directory"
// without the path.
func (f *Frontend) mkdir(w http.ResponseWriter, r *http.Request, rec store.Record, m meta, s session) {
	var req struct {
		Path      string `json:"path"`
		Cwd       string `json:"cwd"`
		Recursive bool   `json:"recursive"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Invalid request: `path` is required.")
		return
	}
	dir := f.resolve(req.Path, req.Cwd)
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		stopped(w)
		return
	}
	defer release()
	argv := []string{"mkdir", "--", dir}
	if req.Recursive {
		argv = []string{"mkdir", "-p", "--", dir}
	}
	_, stderr, code, err := f.runShort(r.Context(), mach, execSpec{argv: argv, dir: "/"})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_server_error", "Failed to create directory: "+err.Error())
		return
	}
	if code != 0 {
		fileErr(w, mkdirMessage(dir, stderr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// mkdirMessage turns mkdir's complaint ("mkdir: cannot create directory
// '/x': File exists") into hosted's message.
func mkdirMessage(dir, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	reason := stderr
	if i := strings.LastIndex(stderr, ": "); i >= 0 {
		reason = stderr[i+2:]
	}
	if reason == "No such file or directory" {
		return "error creating directory: " + reason
	}
	return fmt.Sprintf("error creating directory: %s: %s", dir, reason)
}
