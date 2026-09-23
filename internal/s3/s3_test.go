package s3

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhgaylor/wisp/internal/s3/s3test"
)

func newTestClient(t *testing.T, srv *s3test.Server) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: srv.URL, Bucket: "buck", Region: "home-cloud",
		AccessKey: "GKtest", SecretKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSignatureAWSVector is AWS's published "GET Object" SigV4 example, the whole
// way through: canonical request, string to sign, key derivation, signature. It
// signs a `range` header, which this client never sends, which is why signature()
// takes the canonical pieces instead of an *http.Request.
func TestSignatureAWSVector(t *testing.T) {
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	canonicalHeaders := "host:examplebucket.s3.amazonaws.com\nrange:bytes=0-9\n" +
		"x-amz-content-sha256:" + emptySHA + "\nx-amz-date:20130524T000000Z\n"
	sig, scope := signature("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3",
		"20130524", "20130524T000000Z", http.MethodGet, "/test.txt", "",
		canonicalHeaders, "host;range;x-amz-content-sha256;x-amz-date", emptySHA)

	if want := "20130524/us-east-1/s3/aws4_request"; scope != want {
		t.Errorf("scope = %q, want %q", scope, want)
	}
	if want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"; sig != want {
		t.Errorf("signature\n got %s\nwant %s", sig, want)
	}
}

func TestURIEncode(t *testing.T) {
	// The path case is the canonical URI from AWS's own "PUT Object" example.
	for _, tc := range []struct{ in, want string }{
		{"/examplebucket/test$file.text", "/examplebucket/test%24file.text"},
		{"/buck/chunks/ab/0123abcd", "/buck/chunks/ab/0123abcd"},
		{"/buck/sprites/x/manifests/20260920T224601.000000000Z.json",
			"/buck/sprites/x/manifests/20260920T224601.000000000Z.json"},
		{"/buck/a b~c.d-e_f", "/buck/a%20b~c.d-e_f"},
	} {
		if got := uriEncodePath(tc.in); got != tc.want {
			t.Errorf("uriEncodePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A query value's slashes and plus signs do get encoded: continuation tokens
	// are base64 and would otherwise break the signature.
	if got := canonicalQuery(url.Values{"continuation-token": {"a+b/c="}, "list-type": {"2"}}); got != "continuation-token=a%2Bb%2Fc%3D&list-type=2" {
		t.Errorf("canonicalQuery = %q", got)
	}
}

// TestSignatureGolden pins the whole Authorization header for a fixed clock,
// credential and request. It is a regression guard, not a proof of correctness:
// the proof is a real request to Garage, which scripts/verify-backup.sh makes.
//
// The bucket is a neutral fixture rather than this project's name, because the
// bucket is part of the canonical request: renaming the project would otherwise
// invalidate the signature below and look like a signer regression. The value
// was derived from the SigV4 spec independently of the implementation.
func TestSignatureGolden(t *testing.T) {
	c, err := New(Config{Endpoint: "http://garage-s3:3900", Bucket: "example-bucket",
		Region: "home-cloud", AccessKey: "GKexample", SecretKey: "secretexample"})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return time.Date(2026, 9, 20, 22, 46, 1, 0, time.UTC) }
	req, _ := http.NewRequest(http.MethodPut, c.url("chunks/ab/abcdef", nil).String(), nil)
	c.sign(req, []byte("hello"))

	if got := req.Header.Get("X-Amz-Date"); got != "20260920T224601Z" {
		t.Errorf("x-amz-date = %q", got)
	}
	// sha256("hello")
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("x-amz-content-sha256 = %q", got)
	}
	want := "AWS4-HMAC-SHA256 Credential=GKexample/20260920/home-cloud/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=ee5f64e4e88a07ea316849d7dd4242a7e718af7f6f4801254d01c089e1ceca7f"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	srv := s3test.New("buck")
	defer srv.Close()
	c := newTestClient(t, srv)
	ctx := context.Background()

	if err := c.Put(ctx, "chunks/ab/one", []byte("first")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := c.Get(ctx, "chunks/ab/one")
	if err != nil || string(got) != "first" {
		t.Fatalf("get: %q %v", got, err)
	}
	if n, err := c.Head(ctx, "chunks/ab/one"); err != nil || n != 5 {
		t.Fatalf("head: %d %v", n, err)
	}
	if _, err := c.Get(ctx, "chunks/ab/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: want ErrNotFound, got %v", err)
	}
	if _, err := c.Head(ctx, "chunks/ab/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head missing: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx, "chunks/ab/one"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// A key that is already gone is not an error: garbage collection reruns.
	if err := c.Delete(ctx, "chunks/ab/one"); err != nil {
		t.Fatalf("delete again: %v", err)
	}
}

func TestListPaginates(t *testing.T) {
	srv := s3test.New("buck")
	defer srv.Close()
	srv.SetPageSize(3)
	c := newTestClient(t, srv)
	ctx := context.Background()

	for i := range 10 {
		srv.Put(fmt.Sprintf("chunks/%02d/key", i), []byte("x"))
	}
	srv.Put("sprites/abc/latest.json", []byte("{}"))

	var keys []string
	if err := c.List(ctx, "chunks/", func(o Object) error {
		keys = append(keys, o.Key)
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 10 {
		t.Fatalf("listed %d keys across pages: %v", len(keys), keys)
	}

	// A callback error stops the walk and comes back unwrapped.
	sentinel := errors.New("stop")
	if err := c.List(ctx, "chunks/", func(Object) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("list callback error: %v", err)
	}

	dirs, err := c.ListDirs(ctx, "sprites/")
	if err != nil || len(dirs) != 1 || dirs[0] != "sprites/abc/" {
		t.Fatalf("listdirs: %v %v", dirs, err)
	}
}

func TestErrorCarriesCode(t *testing.T) {
	srv := s3test.New("buck")
	defer srv.Close()
	srv.SetFail(func(method, key string) int {
		if method == http.MethodPut {
			return http.StatusForbidden
		}
		return 0
	})
	c := newTestClient(t, srv)

	err := c.Put(context.Background(), "chunks/ab/one", []byte("x"))
	var se *Error
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Code != "StubFailure" {
		t.Fatalf("want *s3.Error with the code from the body, got %v", err)
	}
}

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.env")
	// The `export ` prefix is what home-cloud's share-file skill writes.
	os.WriteFile(path, []byte("export AWS_ACCESS_KEY_ID=GK123\nexport AWS_SECRET_ACCESS_KEY=\"sh h\"\n"), 0o600)
	access, secret, err := LoadCredentials(path)
	if err != nil || access != "GK123" || secret != "sh h" {
		t.Fatalf("loaded %q %q: %v", access, secret, err)
	}

	os.WriteFile(path, []byte("AWS_ACCESS_KEY_ID=GK456\n"), 0o600)
	if _, _, err := LoadCredentials(path); err == nil {
		t.Fatal("a file with no secret key should be an error")
	}

	t.Setenv("AWS_ACCESS_KEY_ID", "GKenv")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")
	if access, secret, err := LoadCredentials(""); err != nil || access != "GKenv" || secret != "envsecret" {
		t.Fatalf("empty path should read the environment, got %q %q %v", access, secret, err)
	}
}
