package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxBodyBytes bounds pool API request bodies.
const maxBodyBytes = 1 << 16

// ScaleRequest is the body of PUT /pool/scale.
type ScaleRequest struct {
	// Replicas is the desired count; like Pool.Scale, values outside
	// [0, MaxReplicas] are clamped rather than rejected.
	Replicas *int `json:"replicas"`
}

// EndpointsResponse is the body of GET /endpoints.
type EndpointsResponse struct {
	Endpoints []string `json:"endpoints"`
}

// Handler returns the pool API: GET /pool (Status), PUT /pool/scale
// ({"replicas":n}, answers with the new Status), GET /endpoints
// ({"endpoints":[...]}, ready replicas only) and GET /healthz.
func (p *Pool) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /pool", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.Status())
	})
	mux.HandleFunc("PUT /pool/scale", p.handleScale)
	mux.HandleFunc("GET /endpoints", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, EndpointsResponse{Endpoints: p.Endpoints()})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func (p *Pool) handleScale(w http.ResponseWriter, req *http.Request) {
	n, err := decodeScale(w, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := p.Scale(n); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrClosed) {
			code = http.StatusServiceUnavailable
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, p.Status())
}

func decodeScale(w http.ResponseWriter, req *http.Request) (int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}
	var sr ScaleRequest
	if err := json.Unmarshal(body, &sr); err != nil {
		return 0, fmt.Errorf(`decode body: %w (want {"replicas":n})`, err)
	}
	if sr.Replicas == nil {
		return 0, errors.New(`missing "replicas"`)
	}
	return *sr.Replicas, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
