package daytona

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The toolbox's filesystem endpoints, on wisp-agent's fs API.

// agentEntry is an entry of wisp-agent's fs/list and fs/stat.
type agentEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"` // octal permission bits, "0644"
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
	Owner   string    `json:"owner"`
	Group   string    `json:"group"`
}

// fileInfoJSON is FileInfo. mode is the type and permission bits as ls shows
// them (Go's FileMode string, "-rw-r--r--", "drwxr-xr-x"), permissions the
// permission bits in octal ("0644").
type fileInfoJSON struct {
	Name        string `json:"name"`
	IsDir       bool   `json:"isDir"`
	Size        int64  `json:"size"`
	Mode        string `json:"mode"`
	Permissions string `json:"permissions"`
	Owner       string `json:"owner"`
	Group       string `json:"group"`
	ModTime     string `json:"modTime"`
	ModifiedAt  string `json:"modifiedAt"`
	Path        string `json:"path"`
}

func fileInfo(e agentEntry) fileInfoJSON {
	perm, _ := strconv.ParseUint(e.Mode, 8, 32)
	mode := fs.FileMode(perm).Perm()
	switch e.Type {
	case "directory":
		mode |= fs.ModeDir
	case "symlink":
		mode |= fs.ModeSymlink
	}
	t := e.ModTime.UTC().Format(time.RFC3339)
	return fileInfoJSON{Name: e.Name, IsDir: e.IsDir, Size: e.Size, Mode: mode.String(), Permissions: fmt.Sprintf("%04o", perm),
		Owner: e.Owner, Group: e.Group, ModTime: t, ModifiedAt: t, Path: e.Path}
}

// pathParam is a required path query parameter, resolved in the guest.
func (f *Frontend) pathParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	p := r.URL.Query().Get(name)
	if p == "" {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", name+" is required")
		return "", false
	}
	return f.resolve(p), true
}

// listFiles is GET /files: a directory's entries (depth is not supported
// beyond 1: the entries of the directory itself).
func (f *Frontend) listFiles(w http.ResponseWriter, r *http.Request, b *box) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = f.home()
	}
	var out struct {
		Entries []agentEntry `json:"entries"`
	}
	if err := f.agentJSON(r.Context(), b.mach, http.MethodGet, "/fs/list", url.Values{"path": {f.resolve(p)}}, nil, &out); err != nil {
		agentFailed(w, r, err)
		return
	}
	infos := make([]fileInfoJSON, len(out.Entries))
	for i, e := range out.Entries {
		infos[i] = fileInfo(e)
	}
	writeJSON(w, http.StatusOK, infos)
}

func (f *Frontend) fileInfo(w http.ResponseWriter, r *http.Request, b *box) {
	p, ok := f.pathParam(w, r, "path")
	if !ok {
		return
	}
	var e agentEntry
	if err := f.agentJSON(r.Context(), b.mach, http.MethodGet, "/fs/stat", url.Values{"path": {p}}, nil, &e); err != nil {
		agentFailed(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fileInfo(e))
}

// createFolder is POST /files/folder: mkdir -p, with mode (octal, default 0755).
func (f *Frontend) createFolder(w http.ResponseWriter, r *http.Request, b *box) {
	p, ok := f.pathParam(w, r, "path")
	if !ok {
		return
	}
	body := map[string]any{"path": p, "mode": r.URL.Query().Get("mode"), "parents": true}
	if err := f.agentJSON(r.Context(), b.mach, http.MethodPost, "/fs/mkdir", nil, body, nil); err != nil {
		agentFailed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (f *Frontend) deleteFile(w http.ResponseWriter, r *http.Request, b *box) {
	p, ok := f.pathParam(w, r, "path")
	if !ok {
		return
	}
	q := url.Values{"path": {p}, "recursive": {strconv.FormatBool(r.URL.Query().Get("recursive") == "true")}}
	if err := f.agentJSON(r.Context(), b.mach, http.MethodDelete, "/fs/delete", q, nil, nil); err != nil {
		agentFailed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *Frontend) moveFile(w http.ResponseWriter, r *http.Request, b *box) {
	src, ok := f.pathParam(w, r, "source")
	if !ok {
		return
	}
	dst, ok := f.pathParam(w, r, "destination")
	if !ok {
		return
	}
	if err := f.agentJSON(r.Context(), b.mach, http.MethodPost, "/fs/rename", nil, map[string]string{"source": src, "dest": dst}, nil); err != nil {
		agentFailed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// downloadFile is GET /files/download: one file's bytes.
func (f *Frontend) downloadFile(w http.ResponseWriter, r *http.Request, b *box) {
	p, ok := f.pathParam(w, r, "path")
	if !ok {
		return
	}
	resp, err := f.agentDo(r.Context(), b.mach, http.MethodGet, "/fs/read", url.Values{"path": {p}}, nil)
	if err != nil {
		agentFailed(w, r, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": p[strings.LastIndex(p, "/")+1:]}))
	io.Copy(w, resp.Body)
}

// uploadField is a bulk upload's form field name: files[<i>].path or files[<i>].file.
var uploadField = regexp.MustCompile(`^files\[(\d+)\]\.(path|file)$`)

// bulkUpload is POST /files/bulk-upload: a multipart form of files[i].path
// fields and files[i].file parts. Each file is streamed into the guest as it
// arrives (wisp-agent writes it atomically, making parent directories); a
// file part that comes before its path is held in a temporary file until the
// path does.
func (f *Frontend) bulkUpload(w http.ResponseWriter, r *http.Request, b *box) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Expected a multipart/form-data body: "+err.Error())
		return
	}
	paths := map[string]string{}
	held := map[string]*os.File{} // index -> a file part whose path has not come yet
	defer func() {
		for _, t := range held {
			t.Close()
			os.Remove(t.Name())
		}
	}()
	put := func(p string, body io.Reader) error {
		q := url.Values{"path": {f.resolve(p)}, "mkdirParents": {"true"}}
		resp, err := f.agentDo(r.Context(), b.mach, http.MethodPut, "/fs/write", q, body)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "Malformed multipart body: "+err.Error())
			return
		}
		m := uploadField.FindStringSubmatch(part.FormName())
		if m == nil {
			part.Close()
			continue
		}
		i := m[1]
		if m[2] == "path" {
			v, _ := io.ReadAll(io.LimitReader(part, 64<<10))
			paths[i] = string(v)
			if t := held[i]; t != nil {
				delete(held, i)
				t.Seek(0, io.SeekStart)
				err := put(paths[i], t)
				t.Close()
				os.Remove(t.Name())
				if err != nil {
					agentFailed(w, r, err)
					return
				}
			}
			continue
		}
		if p, ok := paths[i]; ok {
			if err := put(p, part); err != nil {
				agentFailed(w, r, err)
				return
			}
			continue
		}
		t, err := os.CreateTemp("", "daytona-upload-*")
		if err == nil {
			held[i] = t
			_, err = io.Copy(t, part)
		}
		if err != nil {
			writeErr(w, r, http.StatusInternalServerError, "", "Could not hold an upload: "+err.Error())
			return
		}
	}
	if len(held) > 0 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "A file part has no files[i].path field")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// bulkDownload is POST /files/bulk-download: {"paths": [...]}, answered with
// a multipart/form-data body of one part per path, in order: name "file"
// with the file's bytes, or name "error" with a JSON error, and the path as
// the filename either way. Each file is streamed from the guest as it is
// written.
func (f *Frontend) bulkDownload(w http.ResponseWriter, r *http.Request, b *box) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, r, http.StatusBadRequest, "BAD_REQUEST", "paths is required")
		return
	}
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", mw.FormDataContentType())
	w.WriteHeader(http.StatusOK)
	for _, p := range req.Paths {
		resp, err := f.agentDo(r.Context(), b.mach, http.MethodGet, "/fs/read", url.Values{"path": {f.resolve(p)}}, nil)
		if err != nil {
			status, code, msg := http.StatusBadGateway, "", err.Error()
			var ae *agentError
			if errors.As(err, &ae) {
				status, msg = ae.status, ae.msg
				code = map[int]string{http.StatusNotFound: "FILE_NOT_FOUND", http.StatusForbidden: "FILE_ACCESS_DENIED"}[status]
				if code == "" {
					code = "BAD_REQUEST"
				}
			}
			h := partHeader("error", p, "application/json")
			pw, _ := mw.CreatePart(h)
			json.NewEncoder(pw).Encode(map[string]any{"message": fmt.Sprintf("%s: %s", p, msg), "statusCode": status, "code": code, "source": "DAYTONA_DAEMON"})
			continue
		}
		h := partHeader("file", p, "application/octet-stream")
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			h.Set("Content-Length", cl)
		}
		pw, _ := mw.CreatePart(h)
		_, err = io.Copy(pw, resp.Body)
		resp.Body.Close()
		if err != nil {
			// Cut off mid-file: end the response here, so that the client sees a
			// truncated body rather than a short file.
			f.log.Warn("bulk download cut off", "id", b.rec.ID, "path", p, "err", err)
			return
		}
	}
	mw.Close()
}

// partHeader is a form-data part's header; mime encodes a filename that is
// not plain ASCII as filename* (RFC 2231), which the SDKs decode.
func partHeader(name, filename, contentType string) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": filename}))
	h.Set("Content-Type", contentType)
	return h
}
