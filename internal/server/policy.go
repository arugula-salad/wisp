package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jhgaylor/mini-sprites/internal/netpolicy"
	"github.com/jhgaylor/mini-sprites/internal/store"
	"github.com/jhgaylor/mini-sprites/internal/vmm"
)

type networkPolicyJSON struct {
	Rules []store.NetworkRule `json:"rules"`
}

func (s *Server) getNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.lookup(w, r)
	if !ok {
		return
	}
	// Rules come back as written, includes unexpanded; no policy is an empty list.
	writeJSON(w, http.StatusOK, networkPolicyJSON{Rules: append([]store.NetworkRule{}, sp.NetworkRules...)})
}

// setNetworkPolicy replaces the policy. {"rules": []} clears it: upstream has no DELETE for this one.
func (s *Server) setNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	var req networkPolicyJSON
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	policy, err := netpolicy.Compile(req.Rules)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_policy", err.Error())
		return
	}
	switch err := s.life.egress.setPolicy(r.PathValue("name"), req.Rules, policy); {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "sprite not found")
	case errors.Is(err, errUnenforceable):
		// Never "accepted but not enforced": the client must know the sprite is not confined.
		s.log.Warn("restrictive network policy refused", "sprite", r.PathValue("name"), "err", err)
		writeErr(w, http.StatusServiceUnavailable, "policy_unenforceable", err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	default:
		s.log.Info("network policy set", "sprite", r.PathValue("name"), "rules", len(req.Rules), "restricted", policy.Restrictive())
		if sp, err := s.store.Get(r.PathValue("name")); err == nil {
			s.life.emit(sp, "policy.changed", map[string]any{"policy": "network", "rules": len(req.Rules), "restricted": policy.Restrictive()})
		}
		go s.life.republishNetworkPolicy(r.PathValue("name"))
		// 204, not the 200 the API reference lists: the official Go SDK treats anything else as failure.
		w.WriteHeader(http.StatusNoContent)
	}
}

// publishNetworkPolicy writes the policy to /.sprite/policy/network.json inside
// a running sprite, where upstream puts it. Best effort: the file is information
// for tools in the guest, enforcement is entirely on the host, and the next wake
// publishes again.
func (l *Lifecycle) publishNetworkPolicy(ctx context.Context, m *vmm.Machine, sp store.Sprite) {
	rules := sp.NetworkRules
	if rules == nil {
		rules = []store.NetworkRule{}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := agentCall(ctx, m, http.MethodPost, "/internal/netpolicy", networkPolicyJSON{Rules: rules}, nil); err != nil {
		l.log.Debug("could not publish the network policy file in the guest", "sprite", sp.Name, "err", err)
	}
}

// republishNetworkPolicy is for a policy change on a sprite that may be running.
func (l *Lifecycle) republishNetworkPolicy(name string) {
	sp, err := l.store.Get(name)
	if err != nil {
		return
	}
	rt := l.rt(sp.ID)
	rt.mu.Lock()
	m := rt.m
	rt.mu.Unlock()
	if m != nil {
		l.publishNetworkPolicy(context.Background(), m, sp)
	}
}
