package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/replica"
	"tenantscaler/internal/workload"
)

// Failure categories counted per target (and named in log lines).
const (
	failLimit     = "client in-flight limit"
	failEncode    = "request encoding"
	failTransport = "transport error"
	failAborted   = "aborted"
	failStream    = "broken stream"
)

// completionRequest is the body of POST /v1/completions.
type completionRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
	IgnoreEOS bool   `json:"ignore_eos"`
	// CachePrompt is always sent as false: it asks llama-server not to
	// reuse the KV cache of a prompt prefix it processed earlier, so a run
	// against long-lived replicas is not served from an earlier run's
	// cache. The gateway forwards it unchanged; the simulated replica
	// ignores it.
	CachePrompt bool `json:"cache_prompt"`
}

// sender sends each request to every target and records the outcomes. It
// is shared by Replay and Live.
type sender struct {
	client    *http.Client
	ownClient bool
	targets   []Target
	urls      []string        // completion URL per target
	keys      []string        // API key per target ("" = none)
	limits    []chan struct{} // in-flight tokens per target; nil = no cap
	seed      int64
	model     string
	logger    *slog.Logger
	onRecord  func(string, eval.Record)
	start     time.Time // offsets are measured from here; set before dispatching
	wg        sync.WaitGroup
	tally     tally
}

// newSender validates targets and prepares the HTTP client.
func newSender(targets []Target, opt Options) (*sender, error) {
	if len(targets) == 0 {
		return nil, errors.New("loadgen: at least one target is required")
	}
	s := &sender{
		client: opt.HTTP, targets: slices.Clone(targets), seed: opt.Seed, model: opt.Model,
		logger: opt.Logger, onRecord: opt.OnRecord,
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if t.Name == "" {
			return nil, fmt.Errorf("loadgen: target %q has an empty name", t.URL)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("loadgen: duplicate target name %q", t.Name)
		}
		seen[t.Name] = true
		u, err := url.Parse(t.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("loadgen: target %q: URL %q is not an http(s) base URL", t.Name, t.URL)
		}
		s.urls = append(s.urls, strings.TrimRight(t.URL, "/")+"/v1/completions")
		s.keys = append(s.keys, t.APIKey)
		var limit chan struct{}
		if opt.MaxInFlight > 0 {
			limit = make(chan struct{}, opt.MaxInFlight)
		}
		s.limits = append(s.limits, limit)
	}
	if s.model == "" {
		s.model = DefaultModel
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns = 1024
		tr.MaxIdleConnsPerHost = 512
		s.client = &http.Client{Transport: tr}
		s.ownClient = true
	}
	return s, nil
}

// closeIdle releases the idle connections of a client the sender created.
func (s *sender) closeIdle() {
	if s.ownClient {
		s.client.CloseIdleConnections()
	}
}

// since is the offset of now from the start of the run.
func (s *sender) since() time.Duration { return time.Since(s.start) }

// dispatch sends one spec to every target concurrently. store, when not
// nil, receives each target's record (by target index) once it is final.
func (s *sender) dispatch(ctx context.Context, id uint64, spec workload.Spec, store func(t int, rec eval.Record)) {
	words := max(spec.InputTokens, 1)
	maxTokens := max(spec.OutputTokens, 1)
	body, err := json.Marshal(completionRequest{
		Model: s.model, Prompt: Prompt(s.seed, id, words), MaxTokens: maxTokens,
		Stream: true, IgnoreEOS: true, CachePrompt: false,
	})
	for t := range s.targets {
		rec := eval.Record{
			ID: id, Tenant: spec.Tenant, Priority: spec.Priority,
			InputTokens: words, OutputTokens: maxTokens,
		}
		if err != nil || !s.acquire(t) {
			rec.Arrival, rec.Failed = s.since(), true
			category := failLimit
			if err != nil {
				category = failEncode
			}
			s.finish(t, outcome{rec: rec, category: category, err: err}, store)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(t)
			s.finish(t, s.send(ctx, t, spec, body, rec), store)
		}()
	}
}

// acquire takes an in-flight token for target t without blocking.
func (s *sender) acquire(t int) bool {
	if s.limits[t] == nil {
		return true
	}
	select {
	case s.limits[t] <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns an in-flight token of target t.
func (s *sender) release(t int) {
	if s.limits[t] != nil {
		<-s.limits[t]
	}
}

// outcome is one request's final record and how it ended.
type outcome struct {
	rec eval.Record
	// category is the failure category; empty for a completed or shed
	// request.
	category string
	err      error
	// simulated reports that the response carried
	// replica.SimulatedHeader: true (the latency came from simulated
	// compute).
	simulated bool
}

// finish counts and publishes a final record.
func (s *sender) finish(t int, o outcome, store func(int, eval.Record)) {
	name := s.targets[t].Name
	if s.tally.add(name, o.rec, o.category, o.simulated) && o.category != "" {
		s.logger.Warn("loadgen: request failed (first of its kind for this target)",
			"target", name, "tenant", o.rec.Tenant, "category", o.category, "err", o.err)
	}
	if store != nil {
		store(t, o.rec)
	}
	if s.onRecord != nil {
		s.onRecord(name, o.rec)
	}
}

// send performs one request starting from rec (identity and nominal token
// counts) and returns the final record with how the request ended.
func (s *sender) send(ctx context.Context, t int, spec workload.Spec, body []byte, rec eval.Record) outcome {
	o := outcome{rec: rec}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.urls[t], bytes.NewReader(body))
	if err != nil {
		o.rec.Arrival = s.since()
		return o.fail(failTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity") // stream chunks as they come
	req.Header.Set(TenantHeader, spec.Tenant)
	req.Header.Set(PriorityHeader, spec.Priority.Class())
	if key := s.keys[t]; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	o.rec.Arrival = s.since()
	resp, err := s.client.Do(req)
	if err != nil {
		return o.fail(abortedOr(ctx, failTransport), err)
	}
	defer resp.Body.Close()
	o.simulated = resp.Header.Get(replica.SimulatedHeader) == "true"

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		o.rec.Shed, o.rec.ShedReason = true, shedReason(resp.Body)
		return o
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return o.fail("http "+resp.Status, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg))))
	}

	res, err := readStream(resp.Body, s.since)
	if err != nil {
		return o.fail(abortedOr(ctx, failStream), err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // allow connection reuse
	// Done is at least 1ns: on a coarse clock (Windows ticks) a request that
	// finished within the run's first tick would otherwise read as
	// unfinished (eval.Record.Completed needs Done > 0).
	o.rec.FirstToken, o.rec.Done = res.firstToken, max(res.done, 1)
	if res.textChunks == 0 {
		o.rec.FirstToken = o.rec.Done // no text at all: the whole wait was "first token"
	}
	o.rec.OutputTokens = res.textChunks
	if res.completionTokens > 0 {
		o.rec.OutputTokens = res.completionTokens
	}
	if res.promptTokens > 0 {
		o.rec.InputTokens = res.promptTokens // measured by the server's tokenizer
	}
	return o
}

// fail marks the request failed with the given category and cause.
func (o outcome) fail(category string, err error) outcome {
	o.rec.Failed, o.category, o.err = true, category, err
	return o
}

// abortedOr returns failAborted when ctx is done (the caller aborted the
// request), otherwise category.
func abortedOr(ctx context.Context, category string) string {
	if ctx.Err() != nil {
		return failAborted
	}
	return category
}

// shedReason extracts the reason of a 429 response body
// {"error":{"type":...,"reason":...}}.
func shedReason(body io.Reader) string {
	var e struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(body, 4096))
	if json.Unmarshal(data, &e) == nil {
		switch {
		case e.Error.Reason != "":
			return e.Error.Reason
		case e.Error.Type != "":
			return e.Error.Type
		}
	}
	return "rate limited (HTTP 429)"
}
