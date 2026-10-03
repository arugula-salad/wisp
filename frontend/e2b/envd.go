package e2b

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arugula-salad/wisp/engine"
	"github.com/arugula-salad/wisp/internal/vmm"
)

// The envd plane: everything the SDK sends to a sandbox rather than to the
// API, proxied into the guest over the engine's port dial (no guest network
// is involved), plus the /init that prepares envd after every start.

// target is a sandbox port a request is for.
type target struct {
	id, port string
	// unmatched is a signed file URL that matched no sandbox's token.
	unmatched bool
	// badPort is a port that is not a number from 1 to 65535.
	badPort bool
}

// portHost is the left-most label of a sandbox port's Host, <port>-<id>, as
// E2B's proxy parses it (shared/pkg/proxy/host.go).
var portHost = regexp.MustCompile(`^([0-9]+)-([a-z0-9]+)$`)

// route decides whether r is for a sandbox, the way E2B's proxy does: a Host
// of <port>-<id>.<anything>; else the E2b-Sandbox-Id / E2b-Sandbox-Port
// headers (what E2B_SANDBOX_URL mode sends with every envd call); else, for a
// signed /files URL (which under E2B_SANDBOX_URL names no sandbox at all), the
// sandbox whose access token the signature was made with. Anything else is
// the control plane.
func (f *Frontend) route(r *http.Request) (target, bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if label, _, ok := strings.Cut(strings.ToLower(host), "."); ok {
		if m := portHost.FindStringSubmatch(label); m != nil {
			return withPort(m[2], m[1]), true
		}
	}
	if id := r.Header.Get("E2b-Sandbox-Id"); id != "" {
		port := r.Header.Get("E2b-Sandbox-Port")
		if port == "" {
			port = EnvdPort
		}
		return withPort(id, port), true
	}
	if r.URL.Path == "/files" && r.URL.Query().Has("signature") {
		if id, ok := f.bySignature(r); ok {
			return target{id: id, port: EnvdPort}, true
		}
		return target{unmatched: true}, true
	}
	return target{}, false
}

// withPort is a target for port as a client wrote it, normalized: the dial and
// the check for envd's internal endpoints must see the same canonical number,
// or "049983" (or anything carrying more than digits) would slip past the check
// and still reach envd, or ask the guest to dial somewhere else entirely.
func withPort(id, port string) target {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return target{id: id, badPort: true}
	}
	return target{id: id, port: strconv.Itoa(n)}
}

// fileSignature is envd's: v1_ and the unpadded base64 SHA-256 of
// "<path>:<read|write>:<user or ”>:<token>[:<expiration>]"
// (envd internal/api/auth.go; the SDKs' sandbox/signature.ts).
func fileSignature(path, op, user, token string, exp string) string {
	parts := []string{path, op, user, token}
	if exp != "" {
		parts = append(parts, exp)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return "v1_" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// bySignature finds the sandbox a signed /files URL was signed for, by
// recomputing the signature with each one's token: under E2B_SANDBOX_URL the
// URL carries nothing else that names it. envd checks the signature (and its
// expiration) again itself.
func (f *Frontend) bySignature(r *http.Request) (string, bool) {
	q := r.URL.Query()
	op := map[string]string{http.MethodGet: "read", http.MethodHead: "read", http.MethodPost: "write"}[r.Method]
	if op == "" {
		return "", false
	}
	sig := q.Get("signature")
	exp := q.Get("signature_expiration")
	if exp != "" {
		if _, err := strconv.ParseInt(exp, 10, 64); err != nil {
			return "", false
		}
	}
	now := time.Now()
	for _, l := range f.sandboxes() {
		if l.m.paused(now) || l.m.AccessToken == "" {
			continue
		}
		want := fileSignature(q.Get("path"), op, q.Get("username"), l.m.AccessToken, exp)
		if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1 {
			return l.rec.ID, true
		}
	}
	return "", false
}

// envdAuthorized reports whether r carries what envd will ask of it: the
// sandbox's access token as X-Access-Token, or an unexpired /files signature
// made with it. envd checks again itself, but only once the sandbox is awake;
// checking here first means traffic without either can't wake a paused
// sandbox, or keep a running one from pausing. User ports are not checked:
// they are public, as on hosted E2B.
func envdAuthorized(r *http.Request, m meta, now time.Time) bool {
	if m.AccessToken == "" {
		return true
	}
	if tok := r.Header.Get("X-Access-Token"); tok != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(m.AccessToken)) == 1
	}
	q := r.URL.Query()
	sig := q.Get("signature")
	op := map[string]string{http.MethodGet: "read", http.MethodHead: "read", http.MethodPost: "write"}[r.Method]
	if sig == "" || op == "" || r.URL.Path != "/files" {
		return false
	}
	exp := q.Get("signature_expiration")
	if exp != "" {
		n, err := strconv.ParseInt(exp, 10, 64)
		if err != nil || now.Unix() > n {
			return false
		}
	}
	want := fileSignature(q.Get("path"), op, q.Get("username"), m.AccessToken, exp)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// envdInternal are envd's endpoints for the orchestrator, never served through
// E2B's public proxy. /init in particular is ours to call (initEnvd).
var envdInternal = map[string]bool{"/init": true, "/freeze": true, "/unfreeze": true,
	"/fsfreeze": true, "/fsthaw": true, "/collapse": true, "/upgrade": true}

// sandboxNotFound is the proxy's answer for a sandbox it cannot route to, as
// hosted E2B's proxy gives it (shared/pkg/proxy/template): 502, which the
// SDK's isRunning() reads as "not running".
func sandboxNotFound(w http.ResponseWriter, id string) {
	writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": id, "message": "The sandbox was not found", "code": http.StatusBadGateway})
}

// serveSandbox proxies r to t's port in the guest, waking a sandbox whose VM
// is down (it is still running as far as E2B is concerned) and holding it
// busy until the response is done, which for a process or watcher stream is
// as long as the stream stays open.
func (f *Frontend) serveSandbox(w http.ResponseWriter, r *http.Request, t target) {
	if t.unmatched {
		writeErr(w, http.StatusUnauthorized, "invalid signature: it matches no running sandbox's access token")
		return
	}
	if t.badPort {
		writeErr(w, http.StatusBadRequest, "invalid sandbox port")
		return
	}
	// The internal-endpoint check below and envd's own router must agree on
	// the path, so it is cleaned once here and forwarded as cleaned.
	if clean := path.Clean("/" + r.URL.Path); clean != r.URL.Path {
		r.URL.Path, r.URL.RawPath = clean, ""
	}
	rec, err := f.store.GetRecord(t.id)
	m, ok := metaOf(rec)
	if err != nil || !ok || m.paused(time.Now()) {
		sandboxNotFound(w, t.id)
		return
	}
	if t.port == EnvdPort && envdInternal[r.URL.Path] {
		writeErr(w, http.StatusNotFound, "not found: "+r.URL.Path+" is envd's internal API")
		return
	}
	if t.port == EnvdPort && !envdAuthorized(r, m, time.Now()) {
		writeErr(w, http.StatusUnauthorized, "unauthorized access, please provide a valid access token or method signing if supported")
		return
	}
	mach, release, err := f.acquire(r.Context(), rec)
	if err != nil {
		f.log.Warn("could not wake a sandbox for its traffic", "id", t.id, "err", err)
		var lim *engine.LimitError
		if errors.As(err, &lim) {
			f.bootFailed(w, err)
			return
		}
		sandboxNotFound(w, t.id)
		return
	}
	defer release()
	// A Connect stream's request is read while its response is being written.
	http.NewResponseController(w).EnableFullDuplex()
	serveStream(w, r, f.sandboxProxy(t, f.dialEnvd(mach)), "the connection to sandbox "+t.id+" ended before the stream completed")
}

// sandboxProxy is the reverse proxy to one port of a running guest. It
// streams both ways, flushing every write, and passes everything through as
// it is: the access token, the SDK's encodings, the Host.
func (f *Frontend) sandboxProxy(t target, dial dialFunc) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "sandbox"
			pr.Out.Host = pr.In.Host
		},
		Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, t.port) }},
		FlushInterval: -1,
		// A stream cut off by a pause is reported here; serveStream ends it.
		ErrorLog: slog.NewLogLogger(f.log.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			f.log.Debug("sandbox port unreachable", "id", t.id, "port", t.port, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"sandboxId": t.id, "port": t.port, "code": http.StatusBadGateway,
				"message": "The sandbox is running but port is not open"})
		},
	}
}

// Connect streams (docs/providers/e2b.md section 3): a server stream is a
// series of envelopes, [flags u8][length u32 BE][message], and ends with one
// flagged end-stream (0x02) whose message is {} or {"error": ...}.
const (
	connectStream    = "application/connect+"
	connectEndStream = 0x02
)

// serveStream runs proxy and makes sure a Connect stream it relays ends with
// an end-stream envelope even when the guest goes away mid-stream (a pause
// suspends the VM under every open process stream): it then ends it with an
// "unavailable" error, as hosted E2B's proxy does, rather than letting the
// client see a stream cut off. A response that is not a Connect stream, or one
// cut inside an envelope, is left cut off.
func serveStream(w http.ResponseWriter, r *http.Request, proxy http.Handler, message string) {
	sw := &streamWriter{ResponseWriter: w}
	defer func() {
		rec := recover()
		if rec != nil {
			if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
		}
		if !sw.endStream(message) && rec != nil {
			panic(rec)
		}
	}()
	proxy.ServeHTTP(sw, r)
}

// streamWriter follows the envelopes of a Connect stream as they are relayed.
type streamWriter struct {
	http.ResponseWriter
	stream bool // a 200 with a Connect stream content type
	env    envelopes
}

// Unwrap lets http.ResponseController reach Flush and the full-duplex switch.
func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamWriter) WriteHeader(status int) {
	w.stream = status == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), connectStream)
	w.ResponseWriter.WriteHeader(status)
}

func (w *streamWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	if w.stream {
		w.env.advance(b[:n])
	}
	return n, err
}

func (w *streamWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

// endStream closes off a Connect stream that has not ended, at an envelope
// boundary. It reports whether the response is now complete: true for a
// stream that ended by itself or was ended here.
func (w *streamWriter) endStream(message string) bool {
	if !w.stream {
		return false
	}
	if w.env.ended {
		return true
	}
	if !w.env.atBoundary() {
		return false
	}
	w.ResponseWriter.Write(endFrame(message))
	http.NewResponseController(w.ResponseWriter).Flush()
	return true
}

// endFrame is an end-stream envelope carrying an "unavailable" error, the
// one Connect clients take for the peer going away.
func endFrame(message string) []byte {
	msg, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "unavailable", "message": message}})
	b := make([]byte, 5, 5+len(msg))
	b[0] = connectEndStream
	binary.BigEndian.PutUint32(b[1:], uint32(len(msg)))
	return append(b, msg...)
}

// envelopes tracks where a stream of envelopes is: inside a header, inside a
// message, or between two, and whether the end-stream one went by.
type envelopes struct {
	hdr   [5]byte
	hdrN  int
	left  int
	flags byte
	ended bool
}

func (e *envelopes) advance(b []byte) {
	for len(b) > 0 {
		if e.left > 0 {
			n := min(len(b), e.left)
			e.left -= n
			b = b[n:]
			if e.left == 0 && e.flags&connectEndStream != 0 {
				e.ended = true
			}
			continue
		}
		n := copy(e.hdr[e.hdrN:], b)
		e.hdrN += n
		b = b[n:]
		if e.hdrN == len(e.hdr) {
			e.hdrN = 0
			e.flags = e.hdr[0]
			e.left = int(binary.BigEndian.Uint32(e.hdr[1:]))
			if e.left == 0 && e.flags&connectEndStream != 0 {
				e.ended = true
			}
		}
	}
}

func (e *envelopes) atBoundary() bool { return e.hdrN == 0 && e.left == 0 }

// dialFunc reaches a port in one guest.
type dialFunc func(ctx context.Context, port string) (net.Conn, error)

// envdDialer is how a VM's ports are reached: the engine's port dial, but a
// field of the Frontend so that tests can stand in for a guest.
type envdDialer func(m *vmm.Machine) dialFunc

func machineDialer(m *vmm.Machine) dialFunc {
	return func(ctx context.Context, port string) (net.Conn, error) { return engine.DialPort(ctx, m, port) }
}

// envdError is envd answering with a status that is not a success.
type envdError struct {
	status int
	body   string
}

func (e *envdError) Error() string { return fmt.Sprintf("envd answered %d: %s", e.status, e.body) }

// envdCall makes one HTTP request to envd in the guest.
func (f *Frontend) envdCall(ctx context.Context, dial dialFunc, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://envd"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Access-Token", token)
	}
	t := &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, EnvdPort) }}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &envdError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// How long the boot hook waits for envd. A cold boot starts it with the
// guest's other services; a warm resume finds it running, and already set up.
const (
	coldInitWait  = 30 * time.Second
	warmInitWait  = 5 * time.Second
	initRetryWait = 50 * time.Millisecond
)

// initRequest is envd's POST /init body (docs/providers/e2b.md section 7).
type initRequest struct {
	AccessToken    string            `json:"accessToken"`
	EnvVars        map[string]string `json:"envVars,omitempty"`
	DefaultUser    string            `json:"defaultUser"`
	DefaultWorkdir string            `json:"defaultWorkdir"`
	Timestamp      string            `json:"timestamp"`
}

// initEnvd is the engine boot hook (engine.OnBoot) that prepares envd in an
// E2B sandbox's VM before anything else reaches it: POST /init with the
// sandbox's access token, env vars, default user and the time. A cold boot
// (including every restore) starts envd with no token, answering anyone, so
// this has to happen first, and a failure fails the start. A warm resume
// finds envd as it was, token and all; its /init is the same token again
// (harmless, and it steps the guest clock) and a failure there is only logged.
func (f *Frontend) initEnvd(ctx context.Context, b engine.Boot) error {
	if b.Record.API != API {
		return nil
	}
	rec := b.Record
	if cur, err := f.store.GetRecord(rec.ID); err == nil {
		rec = cur
	}
	m, ok := metaOf(rec)
	if !ok {
		return errors.New("E2B sandbox without its metadata")
	}
	wait := coldInitWait
	if b.Warm {
		wait = warmInitWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	// What hosted envd puts in every command's environment (envd main.go, and
	// host/mmds.go from the orchestrator's metadata), which -isnotfc leaves
	// out or false; then the sandbox's own.
	env := map[string]string{"E2B_SANDBOX": "true", "E2B_SANDBOX_ID": rec.ID, "E2B_TEMPLATE_ID": m.TemplateID}
	for k, v := range m.EnvVars {
		env[k] = v
	}
	dial := f.dialEnvd(b.Machine)
	start := time.Now()
	var err error
retry:
	for {
		req := initRequest{AccessToken: m.AccessToken, EnvVars: env, DefaultUser: defaultUser,
			DefaultWorkdir: defaultHome, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
		if err = f.envdCall(ctx, dial, http.MethodPost, "/init", "", req, nil); err == nil {
			f.log.Info("envd initialized", "id", rec.ID, "warm", b.Warm, "took", time.Since(start).Round(time.Millisecond))
			return nil
		}
		var ee *envdError
		if errors.As(err, &ee) && ee.status == http.StatusUnauthorized {
			// Something in the guest got to envd first and set a token of its own:
			// whatever answers on 49983 now is not ours to hand the SDK.
			err = fmt.Errorf("envd refused /init (another token was set first): %w", err)
			break
		}
		select {
		case <-ctx.Done():
			break retry
		case <-time.After(initRetryWait):
		}
	}
	if b.Warm {
		f.log.Warn("envd /init after a resume failed; carrying on with the state it kept", "id", rec.ID, "err", err)
		return nil
	}
	return fmt.Errorf("envd did not take /init: %w", err)
}
