package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/policy"
)

// maxPolicyBytes bounds a POST /policy body.
const maxPolicyBytes = 1 << 20

// apiError is the body of every error response:
// {"error":{"type":...,"message":...,"reason":...,"tenant":...}}.
type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	Tenant  string `json:"tenant,omitempty"`
}

// PolicyResponse is the body of a POST /policy answer.
type PolicyResponse struct {
	// Applied is false when the policy was older than the one in force.
	Applied bool `json:"applied"`
	// Version is the policy version in force after the call.
	Version int64 `json:"version"`
	// LatencyMs is the propagation latency of an applied policy.
	LatencyMs float64 `json:"latencyMs"`
}

// handlePostPolicy serves POST /policy: a malformed or invalid policy is
// 400; an older version is 200 with applied false.
func (g *Gateway) handlePostPolicy(w http.ResponseWriter, r *http.Request) {
	received := g.now()
	var p policy.Policy
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPolicyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: "decode policy: " + err.Error()})
		return
	}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: err.Error()})
		return
	}
	applied, version, lat := g.applyPolicy(p, received)
	writeJSON(w, http.StatusOK, PolicyResponse{Applied: applied, Version: version, LatencyMs: ms(lat)})
}

// handleGetPolicy serves GET /policy: the policy in force.
func (g *Gateway) handleGetPolicy(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, g.Policy())
}

// handleStats serves GET /stats?window=: a Go duration ("15s") or a number
// of seconds; absent means Config.StatsWindow.
func (g *Gateway) handleStats(w http.ResponseWriter, r *http.Request) {
	window, err := parseWindow(r.URL.Query().Get("window"))
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, g.Stats(window))
}

// parseWindow parses a stats window; "" means 0 (the default window).
func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		secs, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0, fmt.Errorf("invalid window %q: want a duration such as 15s or a number of seconds", s)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid window %q: must be positive", s)
	}
	return d, nil
}

// handleHealth serves GET /healthz. The gateway is healthy while it runs,
// even with no ready replica: it then queues requests until one appears.
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	n := len(g.endpoints)
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": g.cfg.Mode, "endpoints": n})
}

// writeError writes {"error": e} with the status.
func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]apiError{"error": e})
}

// writeJSON writes v as JSON with the status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client may be gone; nothing to do then
}
