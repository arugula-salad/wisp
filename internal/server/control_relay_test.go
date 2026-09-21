package server

import (
	"net/http/httptest"
	"testing"
)

func TestOffersControl(t *testing.T) {
	req := func(ua string) bool {
		r := httptest.NewRequest("GET", "/v1/sprites/x/control", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		return (&Server{}).offersControl(r)
	}
	for ua, want := range map[string]bool{
		"sprites-go-sdk/1.0":                false, // its ProxyPorts races on a control socket
		"sprites-go-sdk/2.3 (linux; amd64)": false,
		"":                                  true, // raw clients and Node's WebSocket send none
		"node":                              true,
		"Python/3.14 websockets/15.0":       true,
		"my-tool built on sprites-go-sdk":   true, // only the SDK's own prefix is matched
	} {
		if got := req(ua); got != want {
			t.Errorf("User-Agent %q: offered=%v, want %v", ua, got, want)
		}
	}

	r := httptest.NewRequest("GET", "/v1/sprites/x/control", nil)
	r.Header.Set("User-Agent", "sprites-go-sdk/1.0")
	if !(&Server{opts: Options{ControlForGoSDK: true}}).offersControl(r) {
		t.Error("--control-for-go-sdk must offer control to the Go SDK")
	}
}
