// Package loadgen sends multi-tenant LLM traffic to OpenAI-compatible
// targets (a TenantScaler gateway, a baseline gateway, a simulated replica
// or a real llama.cpp llama-server) and records every request's outcome as
// an eval.Record with wall-clock timings measured by the client.
//
// Wall-clock does not mean real inference: a simulated replica answers
// after sleeping for modelled latencies. Responses carrying the header
// X-TenantScaler-Simulated: true are counted per target (Counts.Simulated),
// so results from a simulated pool can be told apart from results from
// llama-server and labelled simulated.
//
// Replay sends a pre-generated trace (workload.Generate) open-loop: each
// request leaves at its scheduled time whatever happened to earlier ones.
// Live generates the default scenario's traffic indefinitely for the demo,
// with an on-demand spike. Both send every request to every target at the
// same moment, so stacks compared side by side see identical traffic.
//
// Requests are POST {target}/v1/completions with the headers
// x-llm-d-inference-fairness-id (tenant) and x-priority (class name) and
// the body {"model","prompt","max_tokens","stream":true,"ignore_eos":true,
// "cache_prompt":false}. ignore_eos makes a real engine generate exactly
// max_tokens tokens, as the simulated engine always does; prompts are
// unique per request (see Prompt) and cache_prompt:false asks llama-server
// not to reuse cached prompt prefixes.
package loadgen

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/workload"
)

// Request headers understood by the TenantScaler gateway (and ignored by
// replicas).
const (
	// TenantHeader carries the tenant (llm-d's fairness ID).
	TenantHeader = "x-llm-d-inference-fairness-id"
	// PriorityHeader carries the priority class name.
	PriorityHeader = "x-priority"
)

// DefaultModel is the model name sent in request bodies. Replicas serve a
// single model and ignore it.
const DefaultModel = "tenantscaler"

// Target is one OpenAI-compatible endpoint that receives the traffic.
type Target struct {
	// Name identifies the target in the results (for example "baseline").
	Name string
	// URL is the base URL, e.g. "http://127.0.0.1:8080"; requests go to
	// URL + "/v1/completions".
	URL string
	// APIKey, when set, is sent as "Authorization: Bearer <key>": the
	// tenant key of a live-demo gateway, which decides the tenant.
	APIKey string
}

// Options tunes Replay and Live.
type Options struct {
	// Speed compresses time: a request scheduled at offset t is sent at
	// t/Speed (Replay), and a Live scenario runs Speed scenario seconds per
	// wall second. 0 means 1 (real time). Measured latencies are never
	// scaled.
	Speed float64
	// HTTP is the client used for requests; nil means a client without a
	// timeout (responses stream for as long as generation takes) that keeps
	// enough idle connections for high request rates. Cancel the context to
	// abort.
	HTTP *http.Client
	// MaxInFlight caps the concurrent requests per target; 0 or less means
	// no cap. The generator stays open-loop: a request that would exceed the
	// cap is not sent and is recorded as Failed (client-side limit).
	MaxInFlight int
	// Seed keys the prompts (see Prompt); Live ignores it and uses its own
	// seed argument. Prompts are unique within a run, but two runs with
	// the same seed send byte-identical prompts. Use a different seed for
	// every run against the same long-lived llama-server replicas (for
	// example the baseline run and then the TenantScaler run), so a later
	// run cannot be served from an earlier run's prompt cache.
	Seed int64
	// Model is the model name in request bodies; empty means DefaultModel.
	Model string
	// OnRecord, when set, is called once per finished request and target
	// (completed, shed or failed), from many goroutines at once; it must be
	// safe for concurrent use and return quickly.
	OnRecord func(target string, rec eval.Record)
	// Logger receives failure warnings and a per-target summary; nil means
	// slog.Default().
	Logger *slog.Logger
	// SpikeTenant names the tenant that Live.Spike controls (Live only;
	// Replay ignores it). It must be a spike tenant of the scenario. Empty
	// means the scenario's only spike tenant; NewLive rejects an empty
	// SpikeTenant when the scenario has several (ReclaimScenario: A and C).
	SpikeTenant string
}

// speed returns the effective time compression.
func (o Options) speed() (float64, error) {
	switch {
	case o.Speed == 0:
		return 1, nil
	case math.IsNaN(o.Speed) || math.IsInf(o.Speed, 0) || o.Speed < 0:
		return 0, fmt.Errorf("loadgen: speed must be a finite number > 0, got %v", o.Speed)
	}
	return o.Speed, nil
}

// Result is the outcome of a replay.
type Result struct {
	// Records holds, per target name, one record per sent spec in spec
	// order (Record.ID = spec index + 1).
	Records map[string][]eval.Record
	// Counts summarises the outcomes per target name; see
	// Counts.IsSimulated.
	Counts map[string]Counts
}

// Simulated reports whether the named target answered from simulated
// compute (see Counts.Simulated).
func (r Result) Simulated(target string) bool { return r.Counts[target].IsSimulated() }

// Replay sends every spec to every target at spec.Offset/Speed after the
// replay starts and returns, per target name, one record per sent spec in
// spec order (Record.ID = spec index + 1). It returns after the last
// request has finished. It is ReplayResult without the counts.
func Replay(ctx context.Context, specs []workload.Spec, targets []Target, opt Options) (map[string][]eval.Record, error) {
	res, err := ReplayResult(ctx, specs, targets, opt)
	return res.Records, err
}

// ReplayResult sends every spec to every target at spec.Offset/Speed after
// the replay starts and returns the records and outcome counts of every
// target. It returns after the last request has finished.
//
// Arrival, FirstToken and Done are measured wall-clock offsets from the
// replay start: Arrival when the request was sent, FirstToken at the first
// SSE chunk carrying text, Done at "data: [DONE]". They are never scaled by
// Speed, so latencies keep their measured values (for a simulated target,
// the simulated engine's modelled latencies; see Result.Simulated). A
// caller using Speed != 1 must therefore
// divide scenario windows (spike start and end, owner return) by Speed
// before computing metrics with eval.Compute.
//
// OutputTokens is the server's final usage.completion_tokens when present,
// otherwise the number of text chunks; for shed and failed requests it is
// the requested count. A 429 response is recorded as Shed with the reason
// from the JSON error body ({"error":{"reason":...}}); any other failure
// (connection error, non-200 status, stream cut before [DONE]) as Failed.
//
// InputTokens is the server's final usage.prompt_tokens when present (its
// tokenizer's count), otherwise the number of prompt words.
//
// When ctx is canceled ReplayResult stops sending, aborts the requests in
// flight (recorded as Failed) and returns the records of the specs it sent
// together with ctx.Err(). Other errors concern invalid arguments.
func ReplayResult(ctx context.Context, specs []workload.Spec, targets []Target, opt Options) (Result, error) {
	speed, err := opt.speed()
	if err != nil {
		return Result{}, err
	}
	s, err := newSender(targets, opt)
	if err != nil {
		return Result{}, err
	}
	defer s.closeIdle()

	order := make([]int, len(specs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(specs[a].Offset, specs[b].Offset) })

	recs := make([][]eval.Record, len(targets))
	for t := range recs {
		recs[t] = make([]eval.Record, len(specs))
	}
	sent := make([]bool, len(specs))
	s.start = time.Now()
	var ctxErr error
	for _, i := range order {
		due := s.start.Add(time.Duration(float64(specs[i].Offset) / speed))
		if ctxErr = sleepUntil(ctx, due); ctxErr != nil {
			break
		}
		sent[i] = true
		s.dispatch(ctx, uint64(i)+1, specs[i], func(t int, rec eval.Record) { recs[t][i] = rec })
	}
	s.wg.Wait()
	s.logSummary("replay finished")

	out := make(map[string][]eval.Record, len(targets))
	for t, tg := range targets {
		kept := make([]eval.Record, 0, len(specs))
		for i, ok := range sent {
			if ok {
				kept = append(kept, recs[t][i])
			}
		}
		out[tg.Name] = kept
	}
	return Result{Records: out, Counts: s.tally.snapshot()}, ctxErr
}

// sleepUntil waits until t or until ctx is done, whichever comes first.
func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
