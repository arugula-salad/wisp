package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// The filesystem API operates on the sprite's disk directly. Relative paths
// resolve against workingDir (default: the sprite user's home). New files and
// directories are owned by the sprite user, so they look like the user made them.

type fsEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"` // "file", "directory" or "symlink"
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // octal permission bits, e.g. "0644"
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

func (s *Server) registerFS(mux *http.ServeMux) {
	mux.HandleFunc("GET /fs/read", s.fsRead)
	mux.HandleFunc("PUT /fs/write", s.fsWrite)
	mux.HandleFunc("GET /fs/list", s.fsList)
	mux.HandleFunc("DELETE /fs/delete", s.fsDelete)
	mux.HandleFunc("POST /fs/rename", s.fsRename)
	mux.HandleFunc("POST /fs/copy", s.fsCopy)
	mux.HandleFunc("POST /fs/chmod", s.fsChmod)
	mux.HandleFunc("POST /fs/chown", s.fsChown)
}

func fsFail(w http.ResponseWriter, path string, err error) {
	status, code := http.StatusInternalServerError, "io_error"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, fs.ErrPermission):
		status, code = http.StatusForbidden, "permission_denied"
	case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTEMPTY):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, errBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	}
	var pe *fs.PathError
	msg := err.Error()
	if errors.As(err, &pe) {
		msg = pe.Err.Error()
	}
	writeJSON(w, status, map[string]string{"error": msg, "code": code, "path": path})
}

var errBadRequest = errors.New("bad request")

func resolvePath(p, workingDir string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w: path is required", errBadRequest)
	}
	if !filepath.IsAbs(p) {
		if workingDir == "" {
			_, workingDir, _ = defaultUser()
		}
		p = filepath.Join(workingDir, p)
	}
	return filepath.Clean(p), nil
}

func queryPath(r *http.Request) (string, error) {
	return resolvePath(r.URL.Query().Get("path"), r.URL.Query().Get("workingDir"))
}

// own hands a path the agent just created (as root) to the sprite user.
func own(path string) {
	if cred, _, _ := defaultUser(); cred != nil {
		os.Lchown(path, int(cred.Uid), int(cred.Gid))
	}
}

// mkdirAllOwned is os.MkdirAll that also chowns only the directories it creates.
func mkdirAllOwned(dir string, perm fs.FileMode) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		missing = append(missing, d)
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for _, d := range missing {
		own(d)
	}
	return nil
}

func entryFor(path string, info fs.FileInfo) fsEntry {
	typ := "file"
	switch {
	case info.IsDir():
		typ = "directory"
	case info.Mode()&fs.ModeSymlink != 0:
		typ = "symlink"
	}
	return fsEntry{Name: info.Name(), Path: path, Type: typ, Size: info.Size(),
		Mode: fmt.Sprintf("%04o", info.Mode().Perm()), ModTime: info.ModTime().UTC(), IsDir: info.IsDir()}
}

func (s *Server) fsRead(w http.ResponseWriter, r *http.Request) {
	path, err := queryPath(r)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil && info.IsDir() {
		err = &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	if err != nil {
		fsFail(w, path, err)
		return
	}
	s.Sessions.touch()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	io.Copy(w, f)
}

func (s *Server) fsWrite(w http.ResponseWriter, r *http.Request) {
	path, err := queryPath(r)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	perm := fs.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		n, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			fsFail(w, path, fmt.Errorf("%w: invalid mode %q", errBadRequest, m))
			return
		}
		perm = fs.FileMode(n).Perm()
	}
	dir := filepath.Dir(path)
	if parseBool(r.URL.Query().Get("mkdirParents")) {
		if err := mkdirAllOwned(dir, 0o755); err != nil {
			fsFail(w, path, err)
			return
		}
	}
	// Write beside the target and rename over it, so readers never see a partial file.
	tmp, err := os.CreateTemp(dir, ".sprite-write-*")
	if err != nil {
		fsFail(w, path, err)
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r.Body)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fsFail(w, path, err)
		return
	}
	// Replacing a file keeps its owner; a new file belongs to the sprite user.
	if st, serr := os.Stat(path); serr == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			os.Chown(tmp.Name(), int(sys.Uid), int(sys.Gid))
		}
	} else {
		own(tmp.Name())
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		fsFail(w, path, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "size": n, "mode": fmt.Sprintf("%04o", perm)})
}

// fsList returns a directory's children, or a single entry for a file.
func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	path, err := queryPath(r)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	entries := []fsEntry{}
	if !info.IsDir() {
		entries = append(entries, entryFor(path, info))
	} else {
		des, err := os.ReadDir(path)
		if err != nil {
			fsFail(w, path, err)
			return
		}
		for _, de := range des {
			if ci, err := de.Info(); err == nil {
				entries = append(entries, entryFor(filepath.Join(path, de.Name()), ci))
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": entries, "count": len(entries)})
}

func (s *Server) fsDelete(w http.ResponseWriter, r *http.Request) {
	path, err := queryPath(r)
	if err != nil {
		fsFail(w, path, err)
		return
	}
	if path == "/" {
		fsFail(w, path, fmt.Errorf("%w: refusing to delete /", errBadRequest))
		return
	}
	if _, err := os.Lstat(path); err != nil {
		fsFail(w, path, err)
		return
	}
	if parseBool(r.URL.Query().Get("recursive")) {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		fsFail(w, path, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "deleted": true})
}

type fsPathsRequest struct {
	Source     string `json:"source"`
	Dest       string `json:"dest"`
	Path       string `json:"path"`
	WorkingDir string `json:"workingDir"`
	Recursive  bool   `json:"recursive"`
	Mode       string `json:"mode"`
	UID        *int   `json:"uid"`
	GID        *int   `json:"gid"`
}

func decodeFSBody(w http.ResponseWriter, r *http.Request) (fsPathsRequest, bool) {
	var req fsPathsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fsFail(w, "", fmt.Errorf("%w: invalid JSON body", errBadRequest))
		return req, false
	}
	return req, true
}

func (s *Server) fsRename(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFSBody(w, r)
	if !ok {
		return
	}
	src, err := resolvePath(req.Source, req.WorkingDir)
	dst, err2 := resolvePath(req.Dest, req.WorkingDir)
	if err = errors.Join(err, err2); err == nil {
		err = os.Rename(src, dst)
	}
	if err != nil {
		fsFail(w, src, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "dest": dst})
}

func (s *Server) fsCopy(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFSBody(w, r)
	if !ok {
		return
	}
	src, err := resolvePath(req.Source, req.WorkingDir)
	dst, err2 := resolvePath(req.Dest, req.WorkingDir)
	if err = errors.Join(err, err2); err != nil {
		fsFail(w, src, err)
		return
	}
	info, err := os.Lstat(src)
	if err == nil && info.IsDir() && !req.Recursive {
		err = fmt.Errorf("%w: source is a directory; set recursive", errBadRequest)
	}
	if err == nil {
		err = copyTree(src, dst)
	}
	if err != nil {
		fsFail(w, src, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"source": src, "dest": dst})
}

// copyTree copies a file, symlink or directory tree, keeping modes and ownership.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err == nil {
				_, err = io.Copy(out, in)
				if cerr := out.Close(); err == nil {
					err = cerr
				}
			}
			in.Close()
			if err != nil {
				return err
			}
		default:
			return nil // sockets, devices, fifos: skip
		}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			os.Lchown(target, int(sys.Uid), int(sys.Gid))
		}
		return nil
	})
}

// walkMaybe applies fn to path, and to everything beneath it when recursive.
func walkMaybe(path string, recursive bool, fn func(string) error) error {
	if !recursive {
		return fn(path)
	}
	return filepath.Walk(path, func(p string, _ fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return fn(p)
	})
}

func (s *Server) fsChmod(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFSBody(w, r)
	if !ok {
		return
	}
	path, err := resolvePath(req.Path, req.WorkingDir)
	mode, perr := strconv.ParseUint(req.Mode, 8, 32)
	if perr != nil {
		err = errors.Join(err, fmt.Errorf("%w: invalid mode %q", errBadRequest, req.Mode))
	}
	if err == nil {
		err = walkMaybe(path, req.Recursive, func(p string) error { return os.Chmod(p, fs.FileMode(mode).Perm()) })
	}
	if err != nil {
		fsFail(w, path, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "mode": fmt.Sprintf("%04o", mode)})
}

func (s *Server) fsChown(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFSBody(w, r)
	if !ok {
		return
	}
	path, err := resolvePath(req.Path, req.WorkingDir)
	if err == nil && req.UID == nil && req.GID == nil {
		err = fmt.Errorf("%w: uid or gid is required", errBadRequest)
	}
	uid, gid := -1, -1 // -1 leaves that id unchanged
	if req.UID != nil {
		uid = *req.UID
	}
	if req.GID != nil {
		gid = *req.GID
	}
	if err == nil {
		err = walkMaybe(path, req.Recursive, func(p string) error { return os.Lchown(p, uid, gid) })
	}
	if err != nil {
		fsFail(w, path, err)
		return
	}
	s.Sessions.touch()
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "uid": uid, "gid": gid})
}
