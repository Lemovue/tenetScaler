package replica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"tenantscaler/internal/engine"
)

// Wire-level constants of the simulated OpenAI-compatible API.
const (
	// DefaultModel is the model name listed by /v1/models and used when a
	// request does not name one.
	DefaultModel = "tenantscaler-simulated-llm"
	// SimulatedHeader is set to "true" on every response of a simulated
	// replica so that nobody mistakes its numbers for real GPU numbers.
	SimulatedHeader = "X-TenantScaler-Simulated"
	// ReplicaHeader carries the replica ID on every response.
	ReplicaHeader = "X-TenantScaler-Replica"
	// DefaultMaxTokens is used when a request omits max_tokens.
	DefaultMaxTokens = 64
	// MaxMaxTokens bounds max_tokens to keep one request from occupying a
	// slot for hours.
	MaxMaxTokens = 32768
	// maxBodyBytes bounds the request body size.
	maxBodyBytes = 1 << 20
	// tokenText is the text of every generated token.
	tokenText = "tok "
)

// CompletionRequest is the body of POST /v1/completions. Prompt must be a
// single string; its token count is engine.CountTokens(Prompt).
type CompletionRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// MaxTokens is the exact number of tokens generated (the simulated
	// engine ignores EOS, like a trace replay); nil means DefaultMaxTokens.
	MaxTokens *int `json:"max_tokens,omitempty"`
	Stream    bool `json:"stream"`
}

// Choice is one completion choice; FinishReason is null until the last
// streamed chunk.
type Choice struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// Usage is the OpenAI token accounting of a completed request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionResponse is a non-streamed response or one streamed SSE chunk.
type CompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Handler returns the replica's HTTP API: POST /v1/completions,
// GET /v1/models, GET /health/ready, GET /health/live and GET /metrics.
// Every response carries SimulatedHeader: true.
func (r *Replica) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/completions", r.handleCompletions)
	mux.HandleFunc("GET /v1/models", r.handleModels)
	mux.HandleFunc("GET /health/ready", r.handleReady)
	mux.HandleFunc("GET /health/live", r.handleLive)
	mux.HandleFunc("GET /metrics", r.handleMetrics)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(SimulatedHeader, "true")
		w.Header().Set(ReplicaHeader, r.id)
		mux.ServeHTTP(w, req)
	})
}

func (r *Replica) handleCompletions(w http.ResponseWriter, req *http.Request) {
	arrival := time.Now()
	creq, err := decodeCompletion(w, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	ctx := req.Context()
	if err := r.admit(ctx); err != nil {
		if errors.Is(err, errUnavailable) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
		}
		return // otherwise the client went away while queued
	}
	defer r.release()

	inTok := engine.CountTokens(creq.Prompt)
	if sleep(ctx, r.prefillDelay(inTok)) != nil {
		return // client disconnected during prefill: abort
	}
	r.metrics.observeFirstToken(inTok, time.Since(arrival))

	g := &generation{
		r: r, ctx: ctx, w: w, req: creq, outTok: *creq.MaxTokens, inTok: inTok,
		id:      fmt.Sprintf("cmpl-%s-%d", r.id, r.reqSeq.Add(1)),
		created: arrival.Unix(),
	}
	if g.run() == nil {
		r.metrics.observeSuccess(time.Since(arrival))
	}
}

// decodeCompletion reads and validates the request body, applying defaults.
// It reads the body to EOF so that the server notices client disconnects.
func decodeCompletion(w http.ResponseWriter, req *http.Request) (CompletionRequest, error) {
	var creq CompletionRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		return creq, fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(body, &creq); err != nil {
		return creq, fmt.Errorf("decode body: %w", err)
	}
	if creq.MaxTokens == nil {
		n := DefaultMaxTokens
		creq.MaxTokens = &n
	}
	if n := *creq.MaxTokens; n < 1 || n > MaxMaxTokens {
		return creq, fmt.Errorf("max_tokens must be in [1, %d], got %d", MaxMaxTokens, n)
	}
	if creq.Model == "" {
		creq.Model = DefaultModel
	}
	return creq, nil
}

// generation produces the output tokens of one admitted request.
type generation struct {
	r       *Replica
	ctx     context.Context
	w       http.ResponseWriter
	req     CompletionRequest
	id      string
	created int64
	inTok   int
	outTok  int
}

// run emits exactly outTok tokens: the first right after prefill, each
// later one after TokenInterval at the running count of that moment. It
// returns an error when the client disconnects.
func (g *generation) run() error {
	if g.req.Stream {
		g.w.Header().Set("Content-Type", "text/event-stream")
		g.w.Header().Set("Cache-Control", "no-cache")
	}
	rc := http.NewResponseController(g.w)
	for i := 1; i <= g.outTok; i++ {
		if i > 1 {
			if err := sleep(g.ctx, g.r.tokenDelay()); err != nil {
				return err
			}
		}
		g.r.metrics.addGenerated(1)
		if !g.req.Stream {
			continue
		}
		chunk := g.response(tokenText, i == g.outTok)
		if err := writeSSE(g.w, chunk); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil {
			return fmt.Errorf("flush: %w", err)
		}
	}
	if g.req.Stream {
		if _, err := io.WriteString(g.w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		return rc.Flush()
	}
	g.w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(g.w).Encode(g.response(strings.Repeat(tokenText, g.outTok), true))
}

// response builds a completion object; the final one carries
// finish_reason "length" and usage.
func (g *generation) response(text string, final bool) CompletionResponse {
	resp := CompletionResponse{
		ID: g.id, Object: "text_completion", Created: g.created, Model: g.req.Model,
		Choices: []Choice{{Index: 0, Text: text}},
	}
	if final {
		reason := "length"
		resp.Choices[0].FinishReason = &reason
		resp.Usage = &Usage{
			PromptTokens: g.inTok, CompletionTokens: g.outTok, TotalTokens: g.inTok + g.outTok,
		}
	}
	return resp
}

func writeSSE(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode chunk: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func (r *Replica) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id": DefaultModel, "object": "model", "owned_by": "tenantscaler-simulated",
		}},
	})
}

func (r *Replica) handleReady(w http.ResponseWriter, _ *http.Request) {
	st := r.State()
	code := http.StatusOK
	if st != StateReady {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"status": st, "replica": r.id})
}

func (r *Replica) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive", "replica": r.id})
}

func (r *Replica) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = r.metrics.write(w, r.Stats())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an OpenAI-style error body.
func writeError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}
