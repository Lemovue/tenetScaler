package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/controller"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/web"
)

const (
	// stopWait bounds how long POST /api/ab/stop waits for the stack to
	// stop before answering 202.
	stopWait = 60 * time.Second
	// defaultDecisionLimit is GET /api/decisions' default limit.
	defaultDecisionLimit = 200
	// pathStart and pathStop are the POST-only endpoints that start and
	// stop an A/B run.
	pathStart = "/api/ab/start"
	pathStop  = "/api/ab/stop"
)

// contentSecurityPolicy allows the dashboard's own files only: no inline
// script, no external origin.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

// staticTypes are the content types of the dashboard's files, set
// explicitly because the operating system's MIME table (the Windows
// registry) may map .js or .css to something else.
var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
}

// Handler serves the dashboard and its API:
//
//	GET  /healthz             {"status":"ok"}
//	POST /api/ab/start        optional body {"seed": n}; 202 {"run","seed"}, 409 while a run is in progress
//	POST /api/ab/stop         200 once the stack has stopped, 409 when nothing runs
//	GET  /api/state           State
//	GET  /api/decisions       ?limit=&tenant= DecisionsReply (TenantScaler controller records)
//	GET  /api/results         Results (real and simulated evaluation summaries)
//	GET  /api/config          ConfigReply
//	GET  /                    the dashboard files (internal/web/static)
//
// Other GET /api/ paths answer a JSON 404, and GET on the POST-only
// endpoints a JSON 405 with Allow: POST.
//
// Non-safe cross-origin browser requests are rejected, so another web
// page cannot start or stop replicas through the viewer's browser.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST "+pathStart, s.handleStart)
	mux.HandleFunc("POST "+pathStop, s.handleStop)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.State()) })
	mux.HandleFunc("GET /api/decisions", s.handleDecisions)
	mux.HandleFunc("GET /api/results", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, LoadResults(s.opt.ResultsDir))
	})
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Config()) })
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathStart, pathStop:
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, r.URL.Path+" accepts POST only")
		default:
			writeError(w, http.StatusNotFound, "no API endpoint "+r.URL.Path)
		}
	})
	mux.Handle("GET /", staticFiles(web.Handler()))
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

// handleStart starts an A/B run.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed *int64 `json:"seed"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid body: want nothing or {\"seed\": <integer>}: "+err.Error())
		return
	}
	seed := s.opt.Seed
	if body.Seed != nil {
		seed = *body.Seed
	}
	run, err := s.StartAB(seed)
	switch {
	case errors.Is(err, ErrBusy):
		writeError(w, http.StatusConflict, "an A/B run is already in progress: stop it first")
	case errors.Is(err, ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "the demo server is shutting down")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]int64{"run": int64(run), "seed": seed})
	}
}

// handleStop stops the running A/B run and waits for its stack to stop.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), stopWait)
	defer cancel()
	err := s.StopAB(ctx)
	switch {
	case errors.Is(err, ErrNotRunning):
		writeError(w, http.StatusConflict, "no A/B run is in progress")
	case err != nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"phase": s.State().Phase, "message": "still stopping the stack"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"phase": s.State().Phase})
	}
}

// handleDecisions serves the TenantScaler controller's decision records.
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := defaultDecisionLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > controller.DefaultLogCapacity {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be an integer in [1, %d]", controller.DefaultLogCapacity))
			return
		}
		limit = n
	}
	tenant := q.Get("tenant")
	if tenant != "" && !s.knownTenant(tenant) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown tenant %q", tenant))
		return
	}
	writeJSON(w, http.StatusOK, s.Decisions(limit, tenant))
}

// knownTenant reports whether name is a configured tenant or the pool's
// own decision records (control.PoolRecordTenant).
func (s *Server) knownTenant(name string) bool {
	if name == control.PoolRecordTenant {
		return true
	}
	for _, t := range s.opt.Config.Tenants {
		if t.Name == name {
			return true
		}
	}
	return false
}

// StackInfo describes one side of the A/B comparison.
type StackInfo struct {
	Key          string `json:"key"`
	Setup        string `json:"setup"`
	Title        string `json:"title"`
	Gateway      string `json:"gateway"`
	ControlPlane string `json:"controlPlane"`
}

// ConfigReply is what GET /api/config serves.
type ConfigReply struct {
	Backend  BackendInfo  `json:"backend"`
	Scenario ScenarioInfo `json:"scenario"`
	Stacks   []StackInfo  `json:"stacks"`
	// CapacityReplicas is each stack's replica budget, InitialReplicas the
	// warm pool started before traffic.
	CapacityReplicas int `json:"capacityReplicas"`
	MinReplicas      int `json:"minReplicas"`
	InitialReplicas  int `json:"initialReplicas"`
	// SlotsPerReplica is the gateway's dispatch window per replica and
	// CycleSeconds the control cycle of both autoscalers.
	SlotsPerReplica    int     `json:"slotsPerReplica"`
	CycleSeconds       float64 `json:"cycleSeconds"`
	WindowSeconds      float64 `json:"windowSeconds"`
	SampleEverySeconds float64 `json:"sampleEverySeconds"`
	CooldownSeconds    float64 `json:"cooldownSeconds"`
	Seed               int64   `json:"seed"`
	// PoolRecordTenant is the tenant name of the controller's pool-level
	// decision records (sum of grants, budget clamp, stabilizer).
	PoolRecordTenant string `json:"poolRecordTenant"`
}

// Config describes the comparison the server runs.
func (s *Server) Config() ConfigReply {
	c := s.opt.Config
	reply := ConfigReply{
		Backend: s.opt.Backend, Scenario: s.scenario,
		CapacityReplicas: c.Pool.CapacityReplicas, MinReplicas: c.Pool.MinReplicas, InitialReplicas: s.opt.InitialReplicas,
		SlotsPerReplica: c.SlotsPerReplica(), CycleSeconds: c.Controller.CycleSeconds,
		WindowSeconds: s.opt.Window.Seconds(), SampleEverySeconds: s.opt.SampleEvery.Seconds(),
		CooldownSeconds: s.opt.Cooldown.Seconds(), Seed: s.opt.Seed, PoolRecordTenant: control.PoolRecordTenant,
	}
	for _, p := range abStacks {
		gw, ctl, _ := stack.Describe(p.setup)
		reply.Stacks = append(reply.Stacks, StackInfo{Key: p.key, Setup: p.setup, Title: p.title, Gateway: gw, ControlPlane: ctl})
	}
	return reply
}

// staticFiles serves the dashboard files with explicit content types and
// revalidation on every load.
func staticFiles(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path
		if strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		if ct, ok := staticTypes[strings.ToLower(path.Ext(name))]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// securityHeaders adds the headers every response carries.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// writeJSON writes v as a JSON response that is never cached.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		data, _ = json.Marshal(map[string]string{"error": "encode response: " + err.Error()})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeError writes {"error": msg} with status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
