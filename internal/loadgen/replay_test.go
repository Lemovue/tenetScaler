package loadgen

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
	"tenantscaler/internal/replica"
	"tenantscaler/internal/workload"
)

func TestReplayStreamsEverySpecToEveryTarget(t *testing.T) {
	x := &fakeLLM{firstDelay: 10 * time.Millisecond, tokenDelay: time.Millisecond}
	y := &fakeLLM{firstDelay: 10 * time.Millisecond, tokenDelay: time.Millisecond}
	targets := []Target{{Name: "x", URL: serve(t, x).URL}, {Name: "y", URL: serve(t, y).URL + "/"}}
	specs := []workload.Spec{ // deliberately not sorted by Offset
		{Offset: 0, Tenant: "A", Priority: model.PriorityCritical, InputTokens: 5, OutputTokens: 3},
		{Offset: 60 * time.Millisecond, Tenant: "B", Priority: model.PriorityStandard, InputTokens: 8, OutputTokens: 4},
		{Offset: 30 * time.Millisecond, Tenant: "C", Priority: model.PrioritySheddable, InputTokens: 1, OutputTokens: 1},
		{Offset: 15 * time.Millisecond, Tenant: "A", Priority: model.PriorityStandard, InputTokens: 3, OutputTokens: 2},
	}
	var mu sync.Mutex
	callbacks := map[string]int{}
	out, err := Replay(context.Background(), specs, targets, Options{
		Seed: 7, Logger: discard,
		OnRecord: func(target string, _ eval.Record) { mu.Lock(); callbacks[target]++; mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, tg := range targets {
		recs := out[tg.Name]
		if len(recs) != len(specs) {
			t.Fatalf("target %s: %d records, want %d", tg.Name, len(recs), len(specs))
		}
		if callbacks[tg.Name] != len(specs) {
			t.Errorf("target %s: OnRecord called %d times, want %d", tg.Name, callbacks[tg.Name], len(specs))
		}
		for i, r := range recs {
			sp := specs[i]
			if r.ID != uint64(i+1) || r.Tenant != sp.Tenant || r.Priority != sp.Priority {
				t.Errorf("%s[%d]: got id %d %s/%v, want id %d %s/%v", tg.Name, i, r.ID, r.Tenant, r.Priority, i+1, sp.Tenant, sp.Priority)
			}
			if !r.Completed() || r.Shed || r.Failed {
				t.Fatalf("%s[%d]: not completed: %+v", tg.Name, i, r)
			}
			if r.Arrival < sp.Offset || r.FirstToken < r.Arrival+10*time.Millisecond || r.Done < r.FirstToken {
				t.Errorf("%s[%d]: timing out of order: offset %v arrival %v first %v done %v",
					tg.Name, i, sp.Offset, r.Arrival, r.FirstToken, r.Done)
			}
			if r.InputTokens != sp.InputTokens || r.OutputTokens != sp.OutputTokens {
				t.Errorf("%s[%d]: tokens %d/%d, want %d/%d", tg.Name, i, r.InputTokens, r.OutputTokens, sp.InputTokens, sp.OutputTokens)
			}
		}
	}

	prompts := map[string]bool{}
	for _, srv := range []*fakeLLM{x, y} {
		got := srv.requests()
		if len(got) != len(specs) {
			t.Fatalf("server saw %d requests, want %d", len(got), len(specs))
		}
		for _, g := range got {
			b := g.Body
			if b.Model != DefaultModel || !b.Stream || !b.IgnoreEOS {
				t.Errorf("body %+v: want model %q, stream and ignore_eos", b, DefaultModel)
			}
			if v, ok := g.Raw["cache_prompt"]; !ok || v != false {
				t.Errorf("body %v: want an explicit \"cache_prompt\": false", g.Raw)
			}
			found := false
			for _, sp := range specs {
				if g.Tenant == sp.Tenant && g.Priority == sp.Priority.Class() &&
					b.MaxTokens == sp.OutputTokens && engine.CountTokens(b.Prompt) == sp.InputTokens {
					found = true
				}
			}
			if !found {
				t.Errorf("request %+v matches no spec", g)
			}
			prompts[b.Prompt] = true
		}
	}
	if len(prompts) != len(specs) {
		t.Errorf("%d distinct prompts over both targets, want %d (unique per spec, same on every target)", len(prompts), len(specs))
	}
}

func TestReplayTokenCounts(t *testing.T) {
	tests := []struct {
		name          string
		srv           *fakeLLM
		input, output int
	}{
		{"usage wins over words and chunk count", &fakeLLM{tokensPerChunk: 2, extraPrompt: 2}, 6, 5},
		{"words and chunk count without usage", &fakeLLM{tokensPerChunk: 2, noUsage: true, extraPrompt: 2}, 4, 3},
		{"EOF after finish_reason counts as done", &fakeLLM{noDone: true}, 4, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs := []workload.Spec{{Tenant: "A", InputTokens: 4, OutputTokens: 5}}
			out, err := Replay(context.Background(), specs, []Target{{Name: "t", URL: serve(t, tc.srv).URL}}, Options{Logger: discard})
			if err != nil {
				t.Fatal(err)
			}
			r := out["t"][0]
			if !r.Completed() || r.InputTokens != tc.input || r.OutputTokens != tc.output {
				t.Errorf("got completed=%v tokens %d/%d, want completed with %d/%d",
					r.Completed(), r.InputTokens, r.OutputTokens, tc.input, tc.output)
			}
		})
	}
}

func TestReplayAgainstSimulatedReplica(t *testing.T) {
	params := engine.DefaultParams()
	params.StartupDuration = 0
	rep := replica.New("r", params, 1)
	rep.Start(time.Now())
	targets := []Target{{Name: "sim", URL: serve(t, rep.Handler()).URL}, {Name: "llm", URL: serve(t, &fakeLLM{}).URL}}
	specs := []workload.Spec{
		{Offset: 0, Tenant: "A", Priority: model.PriorityStandard, InputTokens: 5, OutputTokens: 3},
		{Offset: 10 * time.Millisecond, Tenant: "C", Priority: model.PrioritySheddable, InputTokens: 2, OutputTokens: 4},
	}
	res, err := ReplayResult(context.Background(), specs, targets, Options{Seed: 3, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Records["sim"] {
		sp := specs[i]
		if !r.Completed() || r.InputTokens != sp.InputTokens || r.OutputTokens != sp.OutputTokens {
			t.Errorf("sim[%d] = %+v, want completed with %d/%d tokens", i, r, sp.InputTokens, sp.OutputTokens)
		}
		if r.Arrival < sp.Offset || r.FirstToken <= r.Arrival || r.Done <= r.FirstToken {
			t.Errorf("sim[%d]: timing out of order: arrival %v first %v done %v", i, r.Arrival, r.FirstToken, r.Done)
		}
	}
	if c := res.Counts["sim"]; !res.Simulated("sim") || c.Simulated != len(specs) || c.Completed != len(specs) {
		t.Errorf("sim counts %+v: want %d completed responses, all marked simulated", c, len(specs))
	}
	if res.Simulated("llm") || res.Counts["llm"].Simulated != 0 {
		t.Errorf("llm counts %+v: a target without the simulated header was marked simulated", res.Counts["llm"])
	}
}

func TestReplaySpeedCompressesScheduleNotLatency(t *testing.T) {
	srv := &fakeLLM{firstDelay: 20 * time.Millisecond}
	specs := []workload.Spec{
		{Offset: 0, Tenant: "A", InputTokens: 2, OutputTokens: 1},
		{Offset: 400 * time.Millisecond, Tenant: "A", InputTokens: 2, OutputTokens: 1},
	}
	out, err := Replay(context.Background(), specs, []Target{{Name: "t", URL: serve(t, srv).URL}}, Options{Speed: 4, Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	r := out["t"][1]
	if r.Arrival < 100*time.Millisecond || r.Arrival >= 400*time.Millisecond {
		t.Errorf("second arrival at %v, want about 400ms/4 = 100ms", r.Arrival)
	}
	if r.TTFT() < 20*time.Millisecond {
		t.Errorf("TTFT %v was scaled; want the real >= 20ms", r.TTFT())
	}
}

func TestReplayShedReason(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"gateway JSON", `{"error":{"type":"rate_limited","reason":"tenant over share","tenant":"C"}}`, "tenant over share"},
		{"type only", `{"error":{"type":"rate_limited"}}`, "rate_limited"},
		{"not JSON", `slow down`, "rate limited (HTTP 429)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(tc.body))
			}))
			specs := []workload.Spec{{Tenant: "C", Priority: model.PrioritySheddable, InputTokens: 3, OutputTokens: 7}}
			out, err := Replay(context.Background(), specs, []Target{{Name: "gw", URL: srv.URL}}, Options{Logger: discard})
			if err != nil {
				t.Fatal(err)
			}
			r := out["gw"][0]
			if !r.Shed || r.Failed || r.ShedReason != tc.want {
				t.Errorf("got shed=%v failed=%v reason %q, want shed with reason %q", r.Shed, r.Failed, r.ShedReason, tc.want)
			}
			if r.FirstToken != 0 || r.Done != 0 || r.OutputTokens != 7 {
				t.Errorf("shed record %+v: want zero FirstToken/Done and the requested 7 output tokens", r)
			}
		})
	}
}

func TestReplayFailures(t *testing.T) {
	tests := []struct {
		name string
		url  func(t *testing.T) string
	}{
		{"server error status", func(t *testing.T) string {
			return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			})).URL
		}},
		{"stream cut before done", func(t *testing.T) string { return serve(t, &fakeLLM{cutAfterFirst: true}).URL }},
		{"error event", func(t *testing.T) string {
			return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("data: {\"choices\":[{\"text\":\"a\"}]}\n\nerror: {\"message\":\"slot failed\"}\n\n"))
			})).URL
		}},
		{"connection dropped", func(t *testing.T) string {
			return serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			})).URL
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			specs := []workload.Spec{{Tenant: "A", InputTokens: 3, OutputTokens: 4}}
			out, err := Replay(context.Background(), specs, []Target{{Name: "t", URL: tc.url(t)}}, Options{Logger: discard})
			if err != nil {
				t.Fatal(err)
			}
			r := out["t"][0]
			if !r.Failed || r.Shed || r.Completed() || r.FirstToken != 0 || r.Done != 0 {
				t.Errorf("got %+v, want a failed record without timings", r)
			}
		})
	}
}

func TestReplayMaxInFlightFailsInsteadOfQueueing(t *testing.T) {
	release := make(chan struct{})
	srv := &fakeLLM{block: release}
	var once sync.Once
	specs := []workload.Spec{
		{Tenant: "A", InputTokens: 2, OutputTokens: 1},
		{Tenant: "B", InputTokens: 2, OutputTokens: 1},
	}
	out, err := Replay(context.Background(), specs, []Target{{Name: "t", URL: serve(t, srv).URL}}, Options{
		MaxInFlight: 1, Logger: discard,
		OnRecord: func(_ string, r eval.Record) {
			if r.Failed {
				once.Do(func() { close(release) })
			}
		},
	})
	once.Do(func() { close(release) }) // never leave the server blocked
	if err != nil {
		t.Fatal(err)
	}
	if r := out["t"]; !r[0].Completed() || !r[1].Failed {
		t.Errorf("got %+v and %+v, want the first completed and the second failed by the client limit", r[0], r[1])
	}
	if n := len(srv.requests()); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

func TestReplayCancelReturnsSentRecords(t *testing.T) {
	srv := &fakeLLM{}
	specs := []workload.Spec{
		{Offset: 0, Tenant: "A", InputTokens: 2, OutputTokens: 1},
		{Offset: time.Hour, Tenant: "A", InputTokens: 2, OutputTokens: 1},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	out, err := Replay(ctx, specs, []Target{{Name: "t", URL: serve(t, srv).URL}}, Options{Logger: discard})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if r := out["t"]; len(r) != 1 || r[0].ID != 1 || !r[0].Completed() {
		t.Errorf("got %+v, want only the first (completed) record", r)
	}
}

func TestReplayRejectsBadArguments(t *testing.T) {
	ok := []Target{{Name: "t", URL: "http://127.0.0.1:1"}}
	tests := []struct {
		name    string
		targets []Target
		opt     Options
		want    string
	}{
		{"no targets", nil, Options{}, "at least one target"},
		{"empty name", []Target{{URL: "http://127.0.0.1:1"}}, Options{}, "empty name"},
		{"duplicate", []Target{ok[0], ok[0]}, Options{}, "duplicate"},
		{"bad url", []Target{{Name: "t", URL: "127.0.0.1:1"}}, Options{}, "not an http(s) base URL"},
		{"negative speed", ok, Options{Speed: -1}, "speed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Replay(context.Background(), nil, tc.targets, tc.opt)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
