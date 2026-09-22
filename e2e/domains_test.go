//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCustomDomain attaches a domain to a sprite and fetches the sprite through
// it over HTTPS on the daemon's public listener, with a certificate the daemon
// obtained over TLS-ALPN-01. It needs a daemon started against a test CA whose
// DNS sends every name to that listener; scripts/verify-custom-domains.sh sets
// all of that up. The knobs:
//
//	SPRITES_E2E_PUBLIC     the daemon's --public-listen address (host:port)
//	SPRITES_E2E_ACME_ROOT  PEM file of the root the test CA issues under
func TestCustomDomain(t *testing.T) {
	c := client(t)
	public, rootFile := os.Getenv("SPRITES_E2E_PUBLIC"), os.Getenv("SPRITES_E2E_ACME_ROOT")
	if public == "" || rootFile == "" {
		t.Skip("SPRITES_E2E_PUBLIC / SPRITES_E2E_ACME_ROOT not set (see scripts/verify-custom-domains.sh)")
	}
	rootPEM, err := os.ReadFile(rootFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		t.Fatalf("%s holds no certificate", rootFile)
	}
	// Every name goes to the public listener, as DNS would send it.
	https := func(insecure bool) *http.Client {
		return &http.Client{Timeout: time.Minute, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, InsecureSkipVerify: insecure},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", public)
			}}}
	}
	fetch := func(t *testing.T, cl *http.Client, host, token string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("GET https://%s/: %v", host, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	n := time.Now().UnixNano() % 1e9
	name, clone := fmt.Sprintf("e2e-dom-%d", n), fmt.Sprintf("e2e-domc-%d", n)
	domain := fmt.Sprintf("app-%d.example.test", n)
	if _, err := c.CreateSprite(ctx, name, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.DeleteSprite(context.Background(), clone)
		c.DeleteSprite(context.Background(), name)
	})
	out, err := c.Sprite(name).CommandContext(ctx, "bash", "-c", `mkdir -p ~/site && echo "served at a custom domain" > ~/site/index.html &&
		sprite-env services create web --cmd python3 --args "-m,http.server,3000" --dir /home/sprite/site --http-port 3000 --duration 1s`).CombinedOutput()
	if err != nil {
		t.Fatalf("service: %v\n%s", err, out)
	}

	want(t, http.MethodPost, "/v1/sprites/"+name+"/domains", `{"domain":"`+domain+`"}`, http.StatusCreated)

	var st struct {
		Domain, Status, Reason string
		NotAfter               *time.Time `json:"not_after"`
	}
	start := time.Now()
	for {
		body := want(t, http.MethodGet, "/v1/sprites/"+name+"/domains/"+domain, "", http.StatusOK)
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			t.Fatal(err)
		}
		if st.Status == "issued" {
			break
		}
		if st.Status == "error" || time.Since(start) > 2*time.Minute {
			t.Fatalf("certificate for %s: %s", domain, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("certificate for %s issued in %v, expires %v", domain, time.Since(start).Round(time.Millisecond), st.NotAfter)

	t.Run("served over the issued certificate, under the sprite's URL auth", func(t *testing.T) {
		cl := https(false)
		if code, body := fetch(t, cl, domain, os.Getenv("SPRITES_E2E_TOKEN")); code != http.StatusOK || body != "served at a custom domain\n" {
			t.Fatalf("with the token: %d %q", code, body)
		}
		if code, _ := fetch(t, cl, domain, ""); code != http.StatusUnauthorized {
			t.Fatalf("without the token: %d, want 401 (url_settings.auth is sprite)", code)
		}
		want(t, http.MethodPut, "/v1/sprites/"+name, `{"url_settings":{"auth":"public"}}`, http.StatusOK)
		if code, body := fetch(t, cl, domain, ""); code != http.StatusOK || body != "served at a custom domain\n" {
			t.Fatalf("public: %d %q", code, body)
		}
	})

	t.Run("unknown names are still refused", func(t *testing.T) {
		// The CA's root does not vouch for whatever an unknown name is handed...
		req, _ := http.NewRequest(http.MethodGet, "https://stranger.example.test/", nil)
		if resp, err := https(false).Do(req); err == nil {
			resp.Body.Close()
			t.Fatal("an unknown name got a certificate the CA's root verifies")
		}
		// ...and past the certificate there is nothing for it.
		if code, _ := fetch(t, https(true), "stranger.example.test", ""); code != http.StatusNotFound {
			t.Fatalf("unknown host: %d, want 404", code)
		}
	})

	t.Run("a clone does not inherit the domain", func(t *testing.T) {
		if out, err := c.Sprite(name).CommandContext(ctx, "sprite-env", "checkpoints", "create").CombinedOutput(); err != nil {
			t.Fatalf("checkpoint: %v\n%s", err, out)
		}
		want(t, http.MethodPost, "/v1/sprites", `{"name":"`+clone+`","from":{"sprite":"`+name+`"}}`, http.StatusCreated)
		if body := want(t, http.MethodGet, "/v1/sprites/"+clone+"/domains", "", http.StatusOK); !strings.Contains(body, `"domains":[]`) {
			t.Fatalf("clone's domains: %s", body)
		}
		if code, body := api(t, http.MethodPost, "/v1/sprites/"+clone+"/domains", `{"domain":"`+domain+`"}`); code != http.StatusConflict {
			t.Fatalf("the same domain on the clone: %d %s", code, body)
		}
	})

	t.Run("deleting the sprite frees the domain", func(t *testing.T) {
		want(t, http.MethodDelete, "/v1/sprites/"+name, "", http.StatusNoContent)
		if code, _ := fetch(t, https(true), domain, ""); code != http.StatusNotFound {
			t.Fatalf("a deleted sprite's domain: %d, want 404", code)
		}
		want(t, http.MethodPost, "/v1/sprites/"+clone+"/domains", `{"domain":"`+domain+`"}`, http.StatusCreated)
		want(t, http.MethodDelete, "/v1/sprites/"+clone+"/domains/"+domain, "", http.StatusNoContent)
	})
}
