package server

import (
	"errors"
	"net/http"

	"github.com/arugula-salad/wisp/internal/netpolicy"
	"github.com/arugula-salad/wisp/internal/store"
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
	if !readJSON(w, r, 1<<20, &req) {
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
		go s.life.RepublishNetworkPolicy(r.PathValue("name"))
		// 204, not the 200 the API reference lists: the official Go SDK treats anything else as failure.
		w.WriteHeader(http.StatusNoContent)
	}
}
