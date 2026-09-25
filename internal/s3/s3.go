// Package s3 is a minimal S3 client: path-style addressing, SigV4, and the five
// operations the backup tier needs. Hand-written rather than imported because
// aws-sdk-go-v2 would be by far the largest dependency in this tree, and because
// nothing here needs multipart uploads: every object is a 4 MiB chunk or a small
// JSON blob.
//
// Path-style is not a preference: self-hosted stores (Garage, MinIO) are often
// reached at a name whose bucket subdomains do not resolve, where virtual-host
// addressing cannot work.
package s3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is a 404 from Get or Head.
var ErrNotFound = errors.New("object not found")

// Error is any other non-2xx response, with the code S3 put in the body.
type Error struct {
	Op, Key string
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("s3 %s %s: %s", e.Op, e.Key, http.StatusText(e.Status))
	if e.Code != "" {
		s += ": " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

type Config struct {
	Endpoint  string // http://garage-s3:3900
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// HTTP is optional; the default client has no overall timeout because a
	// chunk upload's deadline comes from the caller's context.
	HTTP *http.Client
}

type Client struct {
	endpoint *url.URL
	bucket   string
	region   string
	access   string
	secret   string
	hc       *http.Client

	// now is overridden by tests that pin the signing timestamp.
	now func() time.Time
}

func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("s3: endpoint and bucket are required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: no credentials")
	}
	ep, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: endpoint: %w", err)
	}
	if ep.Host == "" || (ep.Scheme != "http" && ep.Scheme != "https") {
		return nil, fmt.Errorf("s3: endpoint must be http(s)://host[:port], got %q", cfg.Endpoint)
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	return &Client{endpoint: ep, bucket: cfg.Bucket, region: region,
		access: cfg.AccessKey, secret: cfg.SecretKey, hc: hc, now: time.Now}, nil
}

func (c *Client) Bucket() string   { return c.bucket }
func (c *Client) Endpoint() string { return c.endpoint.String() }

// LoadCredentials reads AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from a file of
// KEY=VALUE lines, with or without a leading `export ` (the shape
// home-cloud's share-file skill caches). An empty path falls back to the
// environment, so a systemd unit can use EnvironmentFile instead.
func LoadCredentials(path string) (access, secret string, err error) {
	if path == "" {
		return os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "AWS_ACCESS_KEY_ID":
			access = v
		case "AWS_SECRET_ACCESS_KEY":
			secret = v
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	if access == "" || secret == "" {
		return "", "", fmt.Errorf("%s: no AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY", path)
	}
	return access, secret, nil
}

// url builds the path-style URL for a key. Both Path and RawPath are set so that
// Go emits our RFC3986 encoding rather than its own, which leaves some
// characters (`:` among them) bare and would break the signature.
func (c *Client) url(key string, query url.Values) *url.URL {
	u := *c.endpoint
	path := "/" + c.bucket
	if key != "" {
		path += "/" + key
	}
	u.Path = path
	u.RawPath = uriEncodePath(path)
	if len(query) > 0 {
		u.RawQuery = canonicalQuery(query)
	}
	return &u
}

// do signs and sends one request, retrying a request whose body it can replay
// through transport errors and 5xx. Garage briefly refuses writes while it
// repairs after a node restart, which is exactly the case worth retrying.
func (c *Client) do(ctx context.Context, method, key string, query url.Values, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(key, query).String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		c.sign(req, body)
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 500 {
			return resp, nil
		}
		lastErr = c.respErr(method, key, resp)
		resp.Body.Close()
	}
	return nil, lastErr
}

// respErr drains the body and turns it into an *Error. S3 error bodies are XML;
// a body we cannot parse still leaves the status, which is the part that matters.
func (c *Client) respErr(op, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	e := &Error{Op: op, Key: key, Status: resp.StatusCode}
	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &parsed) == nil {
		e.Code, e.Message = parsed.Code, parsed.Message
	}
	return e
}

// Put stores an object. Objects here are immutable chunks or whole-file JSON, so
// there is no read-modify-write to worry about.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	resp, err := c.do(ctx, http.MethodPut, key, nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return c.respErr("PUT", key, resp)
	}
	io.Copy(io.Discard, resp.Body) // let the connection be reused
	return nil
}

// Get returns the whole object. Every object this package reads is small enough
// to hold in memory by construction.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return nil, c.respErr("GET", key, resp)
	}
	return io.ReadAll(resp.Body)
}

// Head returns the object's size, or ErrNotFound.
func (c *Client) Head(ctx context.Context, key string) (int64, error) {
	resp, err := c.do(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("%s: %w", key, ErrNotFound)
	}
	if resp.StatusCode/100 != 2 {
		return 0, c.respErr("HEAD", key, resp)
	}
	n, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return n, nil
}

// Delete removes an object. A key that is already gone is not an error, which is
// what a resumable garbage collection wants.
func (c *Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return c.respErr("DELETE", key, resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// list is one ListObjectsV2 page.
func (c *Client) list(ctx context.Context, prefix, delim, token string) (*listResult, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delim != "" {
		q.Set("delimiter", delim)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	resp, err := c.do(ctx, http.MethodGet, "", q, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, c.respErr("LIST", prefix, resp)
	}
	var out listResult
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("s3 LIST %s: %w", prefix, err)
	}
	return &out, nil
}

// List calls fn for every object under prefix, following continuation tokens.
// fn returning an error stops the walk and is returned as-is.
func (c *Client) List(ctx context.Context, prefix string, fn func(Object) error) error {
	for token := ""; ; {
		page, err := c.list(ctx, prefix, "", token)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if err := fn(Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified}); err != nil {
				return err
			}
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}

// ListDirs returns the immediate "directories" under prefix, with their trailing
// slash, without listing everything beneath them.
func (c *Client) ListDirs(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	for token := ""; ; {
		page, err := c.list(ctx, prefix, "/", token)
		if err != nil {
			return nil, err
		}
		for _, p := range page.CommonPrefixes {
			out = append(out, p.Prefix)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}
