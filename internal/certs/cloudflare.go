package certs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Cloudflare is a DNSProvider over Cloudflare's API. The token needs Zone:Read
// and DNS:Edit on the zone that holds the sprite domain, and nothing else.
type Cloudflare struct {
	Token string
	API   string // default https://api.cloudflare.com/client/v4
	HTTP  *http.Client
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Cloudflare) call(ctx context.Context, method, path string, body, result any) error {
	api := c.API
	if api == "" {
		api = "https://api.cloudflare.com/client/v4"
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Errors  []cfError       `json:"errors"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s %s: %s", method, path, resp.Status)
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return fmt.Errorf("cloudflare %s %s: %s (code %d)", method, path, env.Errors[0].Message, env.Errors[0].Code)
		}
		return fmt.Errorf("cloudflare %s %s: %s", method, path, resp.Status)
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}

// zoneID finds the zone holding fqdn by trying each parent domain in turn.
func (c *Cloudflare) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		var zones []struct {
			ID string `json:"id"`
		}
		name := strings.Join(labels[i:], ".")
		if err := c.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return "", err
		}
		if len(zones) > 0 {
			return zones[0].ID, nil
		}
	}
	return "", fmt.Errorf("the Cloudflare token sees no zone for %s", fqdn)
}

func (c *Cloudflare) Present(ctx context.Context, fqdn, value string) (func(context.Context) error, error) {
	zone, err := c.zoneID(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	var rec struct {
		ID string `json:"id"`
	}
	// Cloudflare wants TXT content in its quoted presentation form.
	err = c.call(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", map[string]any{
		"type": "TXT", "name": strings.TrimSuffix(fqdn, "."), "content": `"` + value + `"`, "ttl": 60,
		"comment": "mini-sprites ACME challenge; safe to delete",
	}, &rec)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		return c.call(ctx, http.MethodDelete, "/zones/"+zone+"/dns_records/"+rec.ID, nil, nil)
	}, nil
}
