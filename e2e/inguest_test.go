//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sprites "github.com/superfly/sprites-go"
)

// TestInGuestAPI drives sprite-env and /.sprite/api.sock from inside a sprite
// and checks the effects from outside with the SDK, and the other way round.
func TestInGuestAPI(t *testing.T) {
	c := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := fmt.Sprintf("e2e-in-%d", time.Now().UnixNano()%1e9)
	sp, err := c.CreateSprite(ctx, name, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { c.DeleteSprite(context.Background(), name) })

	sh := func(t *testing.T, script string) string {
		t.Helper()
		out, err := sp.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}

	t.Run("installed for the sprite user", func(t *testing.T) {
		out := sh(t, `id -un; command -v sprite-env; stat -c '%U %a' /.sprite/api.sock`)
		if out != "sprite\n/usr/local/bin/sprite-env\nsprite 660\n" {
			t.Fatalf("got %q", out)
		}
	})

	t.Run("services inside and outside agree", func(t *testing.T) {
		out := sh(t, `sprite-env services create web --cmd python3 --args "-m,http.server,3000" --http-port 3000 --env "A=1,B=2" --dir /tmp --duration 1s`)
		if !strings.Contains(out, `"type":"started"`) || !strings.Contains(out, `"type":"complete"`) {
			t.Fatalf("create stream: %s", out)
		}
		svc, err := sp.GetService(ctx, "web")
		if err != nil || svc.State.Status != "running" || svc.Cmd != "python3" || len(svc.Args) != 3 || svc.HTTPPort == nil || *svc.HTTPPort != 3000 {
			t.Fatalf("outside view of a service created inside: %+v %v", svc, err)
		}
		if got := fetchURL(t, name); !strings.Contains(got, "Directory listing") {
			t.Fatalf("sprite URL does not reach the service: %q", got)
		}

		stream, err := sp.CreateServiceWithDuration(ctx, "outside", &sprites.ServiceRequest{Cmd: "sleep", Args: []string{"600"}}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		stream.ProcessAll(func(*sprites.ServiceLogEvent) error { return nil })
		if out := sh(t, `sprite-env services get outside | jq -r .state.status; sprite-env services list | jq -r '.[].name'`); out != "running\noutside\nweb\n" {
			t.Fatalf("inside view of a service created outside: %q", out)
		}

		out = sh(t, `sprite-env services stop outside >/dev/null && sprite-env services get outside | jq -r .state.status
			sprite-env services start outside --duration 0s | jq -r .type | head -1
			sprite-env services signal outside KILL && sprite-env services restart outside --no-stream
			sprite-env services delete outside && sprite-env services get outside; echo "rc=$?"`)
		if want := "stopped\nstarted\nError: service not found (404)\nrc=1\n"; out != want {
			t.Fatalf("lifecycle from inside: %q, want %q", out, want)
		}
		if out := sh(t, `sprite-env services create web2 --cmd sleep --args 1 --http-port 9 2>&1; echo "rc=$?"`); !strings.Contains(out, "(409)") || !strings.HasSuffix(out, "rc=1\n") {
			t.Fatalf("second http_port service: %q", out)
		}
	})

	t.Run("the socket reaches nothing else", func(t *testing.T) {
		out := sh(t, `for p in /exec /v1/exec /fs/read?path=/etc/shadow /internal/poweroff /v1/sprites /v1/sprites/`+name+`/checkpoints; do
			curl -s --unix-socket /.sprite/api.sock -o /dev/null -w '%{http_code} ' http://sprite$p; done`)
		// /v1/sprites exists but is refused: this sprite has no spawn policy (spawn_test.go).
		if out != "404 404 404 404 403 404 " {
			t.Fatalf("got %q", out)
		}
	})

	t.Run("checkpoint and restore from inside", func(t *testing.T) {
		out := sh(t, `echo before > ~/state; sprite-env checkpoints create --comment "from inside"`)
		if !strings.Contains(out, `"type":"complete"`) || !strings.Contains(out, "Checkpoint v1 created") {
			t.Fatalf("create stream: %s", out)
		}
		cps, err := sp.ListCheckpoints(ctx, "")
		if err != nil || len(cps) != 1 || cps[0].ID != "v1" || cps[0].Comment != "from inside" {
			t.Fatalf("outside view: %+v %v", cps, err)
		}
		if out := sh(t, `sprite-env checkpoints list | jq -r '.[].id'; sprite-env checkpoints get v1 | jq -r .comment
			sprite-env curl -s /v1/checkpoints | jq -r length; sprite-env checkpoints restore v7; echo "rc=$?"`); out != "v1\nfrom inside\n1\nError: checkpoint not found (404)\nrc=1\n" {
			t.Fatalf("inside view: %q", out)
		}

		// The restore kills the VM under the command that asked for it, so the exec
		// fails; what must never happen is the command after it running.
		script := `echo after > ~/state; sudo rm -rf /usr/bin/git; sprite-env checkpoints restore v1; echo survived > ~/state`
		out2, err := sp.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
		if err == nil {
			t.Fatalf("exec outlived an in-guest restore: %s", out2)
		}
		if !strings.Contains(string(out2), "this session ends now") {
			t.Errorf("the requester was not told what was about to happen: %q", out2)
		}
		if out := sh(t, `cat ~/state; test -x /usr/bin/git && echo git-is-back; sprite-env services get web | jq -r .state.status`); out != "before\ngit-is-back\nrunning\n" {
			t.Fatalf("after restore: %q", out)
		}
	})

	t.Run("automatic checkpoints", func(t *testing.T) {
		cps, err := sp.ListCheckpoints(ctx, "")
		if err != nil || len(cps) != 1 {
			t.Fatalf("default listing should hide autos: %+v %v", cps, err)
		}
		all, err := sp.ListCheckpointsWithOptions(ctx, sprites.ListCheckpointsOptions{IncludeAuto: true})
		if err != nil || len(all) != 2 || all[0].ID != "auto-1" || !all[0].IsAuto || all[1].IsAuto {
			t.Fatalf("listing with autos: %+v %v", all, err)
		}
		if out := sh(t, `sprite-env checkpoints list | jq -r '.[].id'; echo; sprite-env checkpoints list --include-auto | jq -r '.[].id'`); out != "v1\n\nauto-1\nv1\n" {
			t.Fatalf("inside listing: %q", out)
		}
		// The auto taken before the restore holds what the restore threw away.
		rs, err := sp.RestoreCheckpoint(ctx, "auto-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.ProcessAll(func(*sprites.StreamMessage) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, `cat ~/state; test -x /usr/bin/git || echo git-is-gone`); out != "after\ngit-is-gone\n" {
			t.Fatalf("undoing the restore: %q", out)
		}
		// A checkpoint taken now descends from v1 (autos are not part of the lineage).
		sh(t, `sprite-env checkpoints create >/dev/null`)
		if desc, err := sp.ListCheckpoints(ctx, "v1"); err != nil || len(desc) != 1 || desc[0].ID != "v2" {
			t.Fatalf("history=v1: %+v %v", desc, err)
		}
	})
}
