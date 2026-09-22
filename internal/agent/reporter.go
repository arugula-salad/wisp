package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// Reporter sends service reports to spritesd over the host channel, where
// they become events on the sprite's stream. Reporting is best effort and never
// holds up a service: reports queue in a small buffer and are dropped when it
// is full or spritesd cannot be reached after a few tries.
type Reporter struct {
	client *http.Client
	q      chan ServiceReport
	// retry is the pause before the second try; it doubles after that.
	retry time.Duration
}

const reporterQueue = 256

func NewReporter(hostDial func(ctx context.Context) (net.Conn, error)) *Reporter {
	r := &Reporter{q: make(chan ServiceReport, reporterQueue), retry: 500 * time.Millisecond,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return hostDial(ctx) }}}}
	go r.run()
	return r
}

// Report queues r without blocking.
func (r *Reporter) Report(rep ServiceReport) {
	select {
	case r.q <- rep:
	default:
	}
}

func (r *Reporter) run() {
	for rep := range r.q {
		body, _ := json.Marshal(rep)
		delay := r.retry
		// A suspend in the middle of a post resets the connection, hence the retries.
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				time.Sleep(delay)
				delay *= 2
			}
			resp, err := r.client.Post("http://host/internal/service-event", "application/json", bytes.NewReader(body))
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode < 500 {
				break // delivered, or refused (malformed, rate limited): trying again will not help
			}
		}
	}
}
