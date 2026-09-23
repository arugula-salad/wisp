package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Webhooks are ours too: the operator names URLs (--webhook) and every event
// is POSTed to each as JSON, signed with a shared secret. Each webhook has its
// own bounded queue and one sender, so a slow or dead receiver delays only its
// own deliveries; when its queue is full, new events are dropped and counted.

// WebhookOptions configures the webhooks. No URLs, no webhooks.
type WebhookOptions struct {
	URLs []string
	// Secret keys the HMAC in the signature header.
	Secret string
	// Types limits deliveries to events whose type starts with one of these.
	Types []string
}

const (
	webhookQueue    = 1024
	webhookAttempts = 6 // the first try and five retries: 1s, 2s, 4s, 8s, 16s apart
	webhookTimeout  = 10 * time.Second

	sigHeader = "X-Wisp-Signature"
	tsHeader  = "X-Wisp-Timestamp"
)

type webhook struct {
	url    string
	secret []byte
	types  []string
	log    *slog.Logger
	client *http.Client
	q      chan Event
	// backoff is the delay before the first retry; it doubles after each. Tests shorten it.
	backoff time.Duration

	delivered, failed, dropped atomic.Uint64
	mu                         sync.Mutex
	lastErr                    string
	lastErrAt                  time.Time
	lastDropLog                time.Time
}

// WebhookStatus is one webhook's line in GET /wisp/v1/webhooks.
type WebhookStatus struct {
	URL       string     `json:"url"`
	Queued    int        `json:"queued"`
	Delivered uint64     `json:"delivered"`
	Failed    uint64     `json:"failed"`  // given up on after every attempt, or refused with a 4xx
	Dropped   uint64     `json:"dropped"` // never attempted: the queue was full
	LastError string     `json:"last_error,omitempty"`
	LastErrAt *time.Time `json:"last_error_at,omitempty"`
}

func newWebhook(url string, opts WebhookOptions, log *slog.Logger) *webhook {
	return &webhook{url: url, secret: []byte(opts.Secret), types: opts.Types, log: log,
		client: &http.Client{Timeout: webhookTimeout}, q: make(chan Event, webhookQueue), backoff: time.Second}
}

// startWebhooks subscribes one sender per configured URL to the bus.
func startWebhooks(bus *eventBus, opts WebhookOptions, log *slog.Logger) []*webhook {
	var hooks []*webhook
	for _, u := range opts.URLs {
		h := newWebhook(u, opts, log)
		hooks = append(hooks, h)
		bus.addSink(h.offer)
		go h.run()
		log.Info("webhook enabled", "url", redactURL(u), "types", strings.Join(opts.Types, ","))
	}
	return hooks
}

// offer queues e without ever waiting: it runs under the bus lock.
func (h *webhook) offer(e Event) {
	if len(h.types) > 0 && !(eventFilter{types: h.types}).match(e) {
		return
	}
	select {
	case h.q <- e:
	default:
		h.dropped.Add(1)
		h.mu.Lock()
		quiet := time.Since(h.lastDropLog) < time.Minute
		if !quiet {
			h.lastDropLog = time.Now()
		}
		h.mu.Unlock()
		if !quiet {
			// In a goroutine: the logger may block on a slow stderr, and we hold the bus.
			go h.log.Warn("webhook queue full; dropping events", "url", redactURL(h.url), "dropped_total", h.dropped.Load())
		}
	}
}

func (h *webhook) run() {
	for e := range h.q {
		h.deliver(e)
	}
}

// deliver tries one event until it lands, is refused, or runs out of attempts.
func (h *webhook) deliver(e Event) {
	body, _ := json.Marshal(e)
	delay := h.backoff
	for attempt := 1; ; attempt++ {
		retry, err := h.post(e, body)
		if err == nil {
			h.delivered.Add(1)
			return
		}
		h.mu.Lock()
		h.lastErr, h.lastErrAt = err.Error(), time.Now().UTC()
		h.mu.Unlock()
		if !retry || attempt == webhookAttempts {
			h.failed.Add(1)
			h.log.Warn("webhook delivery failed", "url", redactURL(h.url), "event", e.ID, "type", e.Type, "attempts", attempt, "err", err)
			return
		}
		time.Sleep(delay)
		delay *= 2
	}
}

// post makes one attempt. retry says whether another could go differently:
// network errors, 5xx and 429 may; any other refusal will not.
func (h *webhook) post(e Event, body []byte) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wisp-webhook/1")
	req.Header.Set("X-Wisp-Event", e.Type)
	req.Header.Set("X-Wisp-Event-Id", strconv.FormatUint(e.ID, 10))
	req.Header.Set(tsHeader, ts)
	req.Header.Set(sigHeader, signWebhook(h.secret, ts, body))
	resp, err := h.client.Do(req)
	if err != nil {
		return true, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return true, fmt.Errorf("receiver answered %s", resp.Status)
	}
	return false, fmt.Errorf("receiver refused the event: %s", resp.Status)
}

// signWebhook is the signature header's value: an HMAC-SHA256 over the
// timestamp header, a dot, and the body, so a captured delivery cannot be
// replayed later under a fresh timestamp. Receivers recompute it and compare
// in constant time, and should reject stale timestamps.
func signWebhook(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (h *webhook) status() WebhookStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := WebhookStatus{URL: redactURL(h.url), Queued: len(h.q), Delivered: h.delivered.Load(),
		Failed: h.failed.Load(), Dropped: h.dropped.Load(), LastError: h.lastErr}
	if !h.lastErrAt.IsZero() {
		t := h.lastErrAt
		st.LastErrAt = &t
	}
	return st
}

// redactURL keeps credentials that an operator put in a URL out of logs and responses.
func redactURL(u string) string {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return u
	}
	if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
		rest = "***@" + rest[at+1:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i] + "?***"
	}
	return scheme + "://" + rest
}

func (s *Server) serveWebhookStatus(w http.ResponseWriter, _ *http.Request) {
	out := []WebhookStatus{}
	for _, h := range s.webhooks {
		out = append(out, h.status())
	}
	writeJSON(w, http.StatusOK, out)
}
