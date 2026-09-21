// Package s3test is an in-process stand-in for the S3 API, enough of it for the
// backup tier's tests: object PUT/GET/HEAD/DELETE and ListObjectsV2 with
// prefixes, delimiters and continuation. It is a test helper, never imported by
// spritesd itself.
//
// It deliberately does not check signatures — Garage does that, and the real
// end-to-end run against Garage is what proves the signing code. It does check
// that a request is signed at all and that x-amz-content-sha256 matches the body,
// which catches the mistakes that are easy to make and hard to see.
package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object struct {
	body    []byte
	modTime time.Time
}

// Server is a stub S3 endpoint holding one bucket in memory.
type Server struct {
	*httptest.Server
	bucket string

	mu   sync.Mutex
	objs map[string]object
	// PageSize caps a list response, so a test can exercise continuation without
	// storing a thousand objects.
	pageSize int
	// Fail, when set, is consulted first: a non-zero status short-circuits the
	// request, which is how tests reach the error paths.
	fail func(method, key string) int
	puts int
	gets int
}

// New starts a stub serving one bucket.
func New(bucket string) *Server {
	s := &Server{bucket: bucket, objs: map[string]object{}, pageSize: 1000}
	s.Server = httptest.NewServer(s)
	return s
}

// SetPageSize caps list pages.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// SetFail installs a hook returning an HTTP status to answer with, or 0 to let
// the request through.
func (s *Server) SetFail(fn func(method, key string) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fn
}

// Counts returns how many object PUTs and GETs have been served, which is how
// the incremental-upload tests assert that dedup happened.
func (s *Server) Counts() (puts, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.gets
}

// ResetCounts zeroes the PUT/GET counters.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts, s.gets = 0, 0
}

// Keys returns every stored key, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objs))
	for k := range s.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Get reads an object directly, bypassing HTTP.
func (s *Server) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	return o.body, ok
}

// Put writes an object directly, for setting up a test's starting state.
func (s *Server) Put(key string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = object{body: body, modTime: time.Now().UTC()}
}

// SetModTime backdates an object, so that a garbage collection's grace period can
// be tested without waiting.
func (s *Server) SetModTime(key string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objs[key]
	if ok {
		o.modTime = t
		s.objs[key] = o
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		writeErr(w, http.StatusForbidden, "AccessDenied", "unsigned request")
		return
	}
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		writeErr(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch",
			"x-amz-content-sha256 does not match the body")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.bucket {
		writeErr(w, http.StatusNotFound, "NoSuchBucket", bucket)
		return
	}

	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if status := fail(r.Method, key); status != 0 {
			writeErr(w, status, "StubFailure", "injected by the test")
			return
		}
	}

	if key == "" && r.URL.Query().Get("list-type") == "2" {
		s.serveList(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.mu.Lock()
		s.objs[key] = object{body: body, modTime: time.Now().UTC()}
		s.puts++
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		s.mu.Lock()
		o, ok := s.objs[key]
		if ok && r.Method == http.MethodGet {
			s.gets++
		}
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			w.Write(o.body)
		}
	case http.MethodDelete:
		s.mu.Lock()
		_, ok := s.objs[key]
		delete(s.objs, key)
		s.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

type listResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	Name        string   `xml:"Name"`
	Prefix      string   `xml:"Prefix"`
	Delimiter   string   `xml:"Delimiter,omitempty"`
	MaxKeys     int      `xml:"MaxKeys"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextToken   string   `xml:"NextContinuationToken,omitempty"`
	Contents    []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, delim, after := q.Get("prefix"), q.Get("delimiter"), q.Get("continuation-token")

	s.mu.Lock()
	keys := make([]string, 0, len(s.objs))
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	pageSize := s.pageSize
	s.mu.Unlock()
	sort.Strings(keys)

	// The continuation token is just the key to resume after, which is enough for
	// a stub and keeps pagination observable in a failing test.
	out := listResult{Name: s.bucket, Prefix: prefix, Delimiter: delim, MaxKeys: pageSize}
	seenPrefix := map[string]bool{}
	n := 0
	for _, k := range keys {
		if after != "" && k <= after {
			continue
		}
		if delim != "" {
			if rest := strings.TrimPrefix(k, prefix); strings.Contains(rest, delim) {
				dir := prefix + rest[:strings.Index(rest, delim)+len(delim)]
				if !seenPrefix[dir] {
					seenPrefix[dir] = true
					out.CommonPrefixes = append(out.CommonPrefixes, struct {
						Prefix string `xml:"Prefix"`
					}{dir})
					n++
				}
				if n >= pageSize {
					out.IsTruncated, out.NextToken = true, k
					break
				}
				continue
			}
		}
		s.mu.Lock()
		o := s.objs[k]
		s.mu.Unlock()
		out.Contents = append(out.Contents, struct {
			Key          string    `xml:"Key"`
			Size         int64     `xml:"Size"`
			LastModified time.Time `xml:"LastModified"`
		}{k, int64(len(o.body)), o.modTime})
		n++
		if n >= pageSize {
			out.IsTruncated, out.NextToken = true, k
			break
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(out)
}
