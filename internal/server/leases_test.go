package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/store"
)

func leaseURL(name string) string { return "/mini-sprites/v1/sprites/" + name + "/lease" }

// leaseNow reads the lease the API reports.
func leaseNow(t *testing.T, h http.Handler, name string) leaseJSON {
	t.Helper()
	var out leaseJSON
	if err := json.Unmarshal(status(t, apiCall(t, h, "GET", leaseURL(name), ""), http.StatusOK), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// expire backdates a sprite's lease, which is the one thing no API call will do
// (a deadline in the past is refused as a typo).
func expire(t *testing.T, s *Server, name string, at time.Time) store.Sprite {
	t.Helper()
	sp, err := s.store.Update(name, func(sp *store.Sprite) { sp.ExpiresAt = &at })
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

// collect drains the events published so far.
func collect(sub *eventSub) []Event {
	var out []Event
	for {
		select {
		case e := <-sub.ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

func leaseEvents(s *Server) *eventSub {
	sub, _, _ := s.life.events.subscribe(func(e Event) bool {
		return e.Type == "sprite.expiring" || e.Type == "sprite.expired" || e.Type == "sprite.deleted"
	}, 0, false)
	return sub
}

// restart is a second daemon on the same data directory, as a spritesd stopped
// and started again.
func restart(t *testing.T, s *Server) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(s.opts.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	again := New(s.opts, st, NewLifecycle(s.opts, st, log), log, "tok", "acme", "sprites.localhost", "0")
	return again, again.Handler()
}

func TestASpriteHasNoLeaseUnlessItAsksForOne(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	body := status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"keeper"}`), http.StatusCreated)
	if strings.Contains(string(body), "expires_at") || strings.Contains(string(body), "protected") {
		t.Errorf("a plain create came back with lease fields: %s", body)
	}
	if sp, _ := s.store.Get("keeper"); sp.ExpiresAt != nil || sp.Protected {
		t.Errorf("record = %+v, want no lease", sp)
	}
	if l := leaseNow(t, h, "keeper"); l.ExpiresAt != nil || l.Protected || l.ExpiresIn != nil {
		t.Errorf("lease = %+v, want none", l)
	}
	// And the reaper is not interested in it, however often it runs.
	s.leases.sweep()
	s.leases.sweep()
	if _, err := s.store.Get("keeper"); err != nil {
		t.Fatal("the reaper deleted a sprite that had no lease")
	}
	// Nor does an unrelated update invent one.
	status(t, apiCall(t, h, "PUT", "/v1/sprites/keeper", `{"labels":["x"]}`), http.StatusOK)
	if sp, _ := s.store.Get("keeper"); sp.ExpiresAt != nil {
		t.Errorf("an update that says nothing about the lease gave it one: %+v", sp.ExpiresAt)
	}
}

func TestLeaseOnCreateUpdateAndRenewal(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	body := status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"tmp","ttl_seconds":3600}`), http.StatusCreated)
	var created spriteJSON
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ExpiresAt == nil || time.Until(*created.ExpiresAt) < 59*time.Minute {
		t.Fatalf("created with ttl_seconds: expires_at = %v", created.ExpiresAt)
	}
	sp, _ := s.store.Get("tmp")
	if sp.ExpiresAt == nil || !sp.ExpiresAt.Equal(*created.ExpiresAt) {
		t.Fatalf("the record does not carry the lease it reported: %+v", sp.ExpiresAt)
	}

	// Renewal by deadline, through the extension endpoint.
	far := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	status(t, apiCall(t, h, "POST", leaseURL("tmp"), `{"expires_at":"`+far.Format(time.RFC3339)+`"}`), http.StatusOK)
	if l := leaseNow(t, h, "tmp"); l.ExpiresAt == nil || !l.ExpiresAt.Equal(far) || l.ExpiresIn == nil || *l.ExpiresIn < 47*3600 {
		t.Fatalf("after a renewal: %+v", l)
	}
	// Protection on its own leaves the deadline alone.
	status(t, apiCall(t, h, "POST", leaseURL("tmp"), `{"protected":true}`), http.StatusOK)
	if l := leaseNow(t, h, "tmp"); !l.Protected || l.ExpiresAt == nil || !l.ExpiresAt.Equal(far) {
		t.Fatalf("after protecting: %+v", l)
	}
	// PUT /v1/sprites/{name} carries the same fields, next to the rest of the record.
	status(t, apiCall(t, h, "PUT", "/v1/sprites/tmp", `{"labels":["keep"],"ttl_seconds":60,"protected":false}`), http.StatusOK)
	sp, _ = s.store.Get("tmp")
	if len(sp.Labels) != 1 || sp.Protected || sp.ExpiresAt == nil || time.Until(*sp.ExpiresAt) > time.Minute {
		t.Fatalf("after a PUT: %+v", sp)
	}
	// And DELETE gives the sprite back its ordinary, endless life.
	status(t, apiCall(t, h, "DELETE", leaseURL("tmp"), ""), http.StatusNoContent)
	if l := leaseNow(t, h, "tmp"); l.ExpiresAt != nil || l.Protected {
		t.Fatalf("after clearing: %+v", l)
	}
	s.leases.sweep()
	if _, err := s.store.Get("tmp"); err != nil {
		t.Fatal("a cleared lease still got the sprite reaped")
	}
}

func TestLeaseRequestsThatMakeNoSense(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"x"}`), http.StatusCreated)
	for _, body := range []string{
		`{"ttl_seconds":60,"expires_at":"2099-01-01T00:00:00Z"}`,
		`{"ttl_seconds":-1}`,
		`{"expires_at":"tomorrow"}`,
		`{"expires_at":"1999-01-01T00:00:00Z"}`,
	} {
		if resp := apiCall(t, h, "POST", leaseURL("x"), body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", body, resp.StatusCode)
		}
	}
	// The same refusals on the way in, and nothing is created when they fire.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"y","ttl_seconds":-1}`), http.StatusBadRequest)
	status(t, apiCall(t, h, "GET", "/v1/sprites/y", ""), http.StatusNotFound)
	status(t, apiCall(t, h, "POST", leaseURL("missing"), `{"ttl_seconds":60}`), http.StatusNotFound)
}

func TestTheReaperDeletesExpiredUnprotectedSprites(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	for _, name := range []string{"gone", "safe", "keeper"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`","ttl_seconds":3600}`), http.StatusCreated)
	}
	status(t, apiCall(t, h, "DELETE", leaseURL("keeper"), ""), http.StatusNoContent)
	status(t, apiCall(t, h, "POST", leaseURL("safe"), `{"protected":true}`), http.StatusOK)
	doomed, _ := s.store.Get("gone")
	expire(t, s, "gone", time.Now().Add(-time.Minute))
	expire(t, s, "safe", time.Now().Add(-time.Minute))

	sub := leaseEvents(s)
	s.leases.sweep()

	if _, err := s.store.Get("gone"); err == nil {
		t.Fatal("an expired, unprotected sprite survived the sweep")
	}
	if _, err := os.Stat(s.store.Dir(doomed.ID)); !os.IsNotExist(err) {
		t.Errorf("the sprite's disk is still on the volume: %v", err)
	}
	for _, name := range []string{"safe", "keeper"} {
		if _, err := s.store.Get(name); err != nil {
			t.Errorf("%s was reaped: %v", name, err)
		}
	}
	// Protection holds the deletion off without pretending the lease is still good.
	if l := leaseNow(t, h, "safe"); l.ExpiresAt == nil || !l.Protected || l.ExpiresIn == nil || *l.ExpiresIn > 0 {
		t.Errorf("protected sprite's lease = %+v, want a deadline in the past", l)
	}
	// The reason first, then the deletion every other client already understands.
	var got []string
	for _, e := range collect(sub) {
		got = append(got, e.Type+":"+e.Sprite)
		if e.Type == "sprite.expired" && e.Detail["expires_at"] == nil {
			t.Errorf("sprite.expired without its deadline: %+v", e.Detail)
		}
	}
	if strings.Join(got, " ") != "sprite.expired:gone sprite.deleted:gone" {
		t.Errorf("events = %v", got)
	}
	// Unprotecting hands the sprite back to the reaper.
	status(t, apiCall(t, h, "POST", leaseURL("safe"), `{"protected":false}`), http.StatusOK)
	s.leases.sweep()
	if _, err := s.store.Get("safe"); err == nil {
		t.Fatal("a sprite whose protection was lifted kept its expired lease")
	}
}

func TestLeasesThatRanOutWhileTheDaemonWasDown(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	for _, name := range []string{"gone", "keeper"} {
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`","ttl_seconds":3600}`), http.StatusCreated)
	}
	expire(t, s, "gone", time.Now().Add(-time.Hour))

	// A lease is on the record, so it outlives the daemon that granted it, and
	// the next daemon reaps before it serves anything.
	again, _ := restart(t, s)
	if _, err := again.store.Get("gone"); err == nil {
		t.Fatal("a sprite whose lease ran out during the downtime came back")
	}
	sp, err := again.store.Get("keeper")
	if err != nil {
		t.Fatal(err)
	}
	if sp.ExpiresAt == nil || time.Until(*sp.ExpiresAt) < 59*time.Minute {
		t.Fatalf("the surviving lease did not come back intact: %+v", sp.ExpiresAt)
	}
}

func TestTheExpiringWarningGoesOutOncePerDeadline(t *testing.T) {
	s, h := newOperatorServer(t, Options{LeaseWarning: 10 * time.Minute})
	// Subscribed before the creates: a sprite born inside the warning window is
	// warned about at once rather than at the first sweep, since with a short
	// enough lease there may not be a sweep before it expires.
	sub := leaseEvents(s)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"soon","ttl_seconds":300}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"later","ttl_seconds":3600}`), http.StatusCreated)

	s.leases.sweep()
	s.leases.sweep()
	got := collect(sub)
	if len(got) != 1 || got[0].Type != "sprite.expiring" || got[0].Sprite != "soon" {
		t.Fatalf("want one warning about soon, got %+v", got)
	}
	if in, ok := got[0].Detail["in_ms"].(int64); !ok || in <= 0 || in > 300_000 {
		t.Errorf("detail = %+v", got[0].Detail)
	}

	// A renewal is a new deadline, and is warned about in its own right.
	status(t, apiCall(t, h, "POST", leaseURL("soon"), `{"ttl_seconds":301}`), http.StatusOK)
	s.leases.sweep()
	if got := collect(sub); len(got) != 1 || got[0].Type != "sprite.expiring" {
		t.Fatalf("after a renewal into the warning window: %+v", got)
	}
	// Renewed well past it, there is nothing to warn about any more.
	status(t, apiCall(t, h, "POST", leaseURL("soon"), `{"ttl_seconds":7200}`), http.StatusOK)
	s.leases.sweep()
	if got := collect(sub); len(got) != 0 {
		t.Fatalf("warned about a lease with two hours to run: %+v", got)
	}
}

// The reaper picks candidates from a list read before it waits on each sprite's
// lock, exactly as the warm-TTL janitor does. A renewal that got the lock first
// has already written a new deadline by the time the reaper gets in, and the
// reaper has to see it: deciding on its own stale copy would delete a workspace
// somebody just extended.
func TestARenewalThatLandsFirstBeatsTheReaper(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"game","ttl_seconds":60}`), http.StatusCreated)
	sp := expire(t, s, "game", time.Now().Add(-time.Second))

	rt := s.life.rt(sp.ID)
	rt.mu.Lock() // a renewal (or any other transition) in flight
	done := make(chan struct{})
	go func() {
		s.leases.reap(sp) // the reaper, holding the record it listed
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the reaper deleted the sprite without waiting for its lock")
	case <-time.After(50 * time.Millisecond):
	}
	// What applyLease does before letting go.
	later := time.Now().Add(time.Hour)
	s.store.Update("game", func(sp *store.Sprite) { sp.ExpiresAt = &later })
	rt.mu.Unlock()
	<-done

	if _, err := s.store.Get("game"); err != nil {
		t.Fatal("the reaper deleted a sprite renewed moments before, deciding on a stale lease")
	}
}

// The other order: once a reap has committed, the sprite is on its way out and
// a renewal must be told so rather than writing a lease onto a record that is
// about to go.
func TestARenewalThatLandsAfterTheReapIsRefused(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"game","ttl_seconds":60}`), http.StatusCreated)
	sp := expire(t, s, "game", time.Now().Add(-time.Second))
	s.leases.claim(sp.ID) // the reaper has committed and is deleting

	for _, req := range [][3]string{
		{"POST", leaseURL("game"), `{"ttl_seconds":3600}`},
		{"POST", leaseURL("game"), `{"protected":true}`},
		{"DELETE", leaseURL("game"), ""},
		{"PUT", "/v1/sprites/game", `{"protected":true}`},
	} {
		if resp := apiCall(t, h, req[0], req[1], req[2]); resp.StatusCode != http.StatusConflict {
			t.Errorf("%s %s: %d, want 409", req[0], req[1], resp.StatusCode)
		}
	}
	if cur, _ := s.store.Get("game"); cur.Protected || cur.ExpiresAt == nil || !cur.ExpiresAt.Equal(*sp.ExpiresAt) {
		t.Fatalf("a refused renewal still changed the record: %+v", cur)
	}
	// An update that is not about the lease is none of this mechanism's business.
	status(t, apiCall(t, h, "PUT", "/v1/sprites/game", `{"labels":["a"]}`), http.StatusOK)
}

// Under the real handlers and a real sweep, the two orders above are the only
// two outcomes: a sprite with a lease it can rely on, or no sprite at all.
func TestRenewingWhileTheReaperRunsLeavesNoHalfDeletedSprite(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	for i := 0; i < 25; i++ {
		name := "race"
		status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"`+name+`","ttl_seconds":60}`), http.StatusCreated)
		sp := expire(t, s, name, time.Now().Add(time.Millisecond))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.leases.sweep() }()
		go func() {
			defer wg.Done()
			apiCall(t, h, "POST", leaseURL(name), `{"ttl_seconds":3600}`)
		}()
		wg.Wait()

		cur, err := s.store.Get(name)
		if err != nil {
			if _, err := os.Stat(s.store.Dir(sp.ID)); !os.IsNotExist(err) {
				t.Fatalf("round %d: the sprite is gone but its disk is not: %v", i, err)
			}
			continue
		}
		if cur.ExpiresAt == nil || time.Until(*cur.ExpiresAt) < 59*time.Minute {
			t.Fatalf("round %d: the sprite survived with a lease that had already run out: %+v", i, cur.ExpiresAt)
		}
		if _, err := os.Stat(s.store.Dir(cur.ID)); err != nil {
			t.Fatalf("round %d: the surviving sprite lost its directory: %v", i, err)
		}
		status(t, apiCall(t, h, "DELETE", "/v1/sprites/"+name, ""), http.StatusNoContent)
	}
}

func TestChildrenAreBornWithTheirLobbysLease(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"lobby"}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites/lobby/policy/spawn", `{"enabled":true}`), http.StatusNoContent)

	// Without child_ttl_seconds nothing changes: children are as permanent as before.
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"old-game"}`), http.StatusCreated)
	if sp, _ := s.store.Get("old-game"); sp.ExpiresAt != nil {
		t.Fatalf("a child got a lease nobody configured: %+v", sp.ExpiresAt)
	}

	status(t, apiCall(t, h, "POST", "/v1/sprites/lobby/policy/spawn", `{"enabled":true,"child_ttl_seconds":600}`), http.StatusNoContent)
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"game"}`), http.StatusCreated)
	game, _ := s.store.Get("game")
	if game.ExpiresAt == nil || time.Until(*game.ExpiresAt) > 10*time.Minute || time.Until(*game.ExpiresAt) < 9*time.Minute {
		t.Fatalf("child lease = %+v, want ten minutes", game.ExpiresAt)
	}
	// The lobby may ask for less, and cannot ask for more or for none at all.
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"short","ttl_seconds":60}`), http.StatusCreated)
	if sp, _ := s.store.Get("short"); sp.ExpiresAt == nil || time.Until(*sp.ExpiresAt) > time.Minute {
		t.Errorf("a shorter lease was not honored: %+v", sp.ExpiresAt)
	}
	status(t, fromInside(t, s, "lobby", "POST", "/v1/sprites", `{"name":"greedy","ttl_seconds":86400,"protected":true}`), http.StatusCreated)
	greedy, _ := s.store.Get("greedy")
	if greedy.Protected || greedy.ExpiresAt == nil || time.Until(*greedy.ExpiresAt) > 10*time.Minute {
		t.Fatalf("a child talked its way out of its lease: %+v", greedy)
	}
	// The lobby itself is untouched by its own policy.
	if lobby, _ := s.store.Get("lobby"); lobby.ExpiresAt != nil {
		t.Errorf("the spawner leased itself: %+v", lobby.ExpiresAt)
	}
	status(t, apiCall(t, h, "POST", "/v1/sprites/lobby/policy/spawn", `{"enabled":true,"child_ttl_seconds":-1}`), http.StatusBadRequest)

	// And the reaper frees the slot the child held under max_children.
	expire(t, s, "game", time.Now().Add(-time.Second))
	s.leases.sweep()
	if _, err := s.store.Get("game"); err == nil {
		t.Fatal("an expired child survived")
	}
	if n := len(s.children(mustGet(t, s, "lobby"))); n != 3 {
		t.Errorf("children after the reap = %d, want 3", n)
	}
}

func mustGet(t *testing.T, s *Server, name string) store.Sprite {
	t.Helper()
	sp, err := s.store.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}
