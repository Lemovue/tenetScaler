package realrun

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// driver sends profile requests to one replica through loadgen, one
// request per Replay call, each with its own prompt seed so that every
// prompt is unique.
type driver struct {
	url  string
	hc   *http.Client
	seed int64
	sent atomic.Int64
	// warn receives loadgen's warnings (a failed request and its cause)
	// but not its per-replay summaries.
	warn *slog.Logger
}

// warnOnly forwards records at Warn level and above to another handler.
type warnOnly struct{ slog.Handler }

// Enabled drops records below Warn.
func (h warnOnly) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

// WithAttrs keeps the filter on derived handlers.
func (h warnOnly) WithAttrs(as []slog.Attr) slog.Handler { return warnOnly{h.Handler.WithAttrs(as)} }

// WithGroup keeps the filter on derived handlers.
func (h warnOnly) WithGroup(name string) slog.Handler { return warnOnly{h.Handler.WithGroup(name)} }

// sentRequest is one request with its absolute send and end times and,
// when it failed, the cause.
type sentRequest struct {
	spec       workload.Spec
	sent, end  time.Time
	rec        eval.Record
	arrivalAbs time.Time
	failure    string
}

// send performs one request and returns its record.
func (d *driver) send(ctx context.Context, spec workload.Spec) sentRequest {
	n := d.sent.Add(1)
	spec.Tenant, spec.Priority, spec.Offset = profileTenant, model.PriorityStandard, 0
	r := sentRequest{spec: spec, sent: time.Now()}
	res, _ := loadgen.ReplayResult(ctx, []workload.Spec{spec}, []loadgen.Target{{Name: "replica", URL: d.url}},
		loadgen.Options{HTTP: d.hc, Seed: d.seed*1_000_003 + n, Logger: d.warn})
	r.end = time.Now()
	if recs := res.Records["replica"]; len(recs) == 1 {
		r.rec = recs[0]
	} else {
		r.rec = eval.Record{Tenant: profileTenant, Failed: true}
		r.failure = "not sent"
	}
	for cause := range res.Counts["replica"].FailedBy {
		r.failure = cause
	}
	r.arrivalAbs = r.sent.Add(r.rec.Arrival)
	return r
}

// prefill runs one warm-up request and then n sequential requests with a
// prompt of the given length and a short answer.
func (d *driver) prefill(ctx context.Context, words, n int) (PrefillPoint, error) {
	pt := PrefillPoint{Words: words}
	spec := workload.Spec{InputTokens: words, OutputTokens: probeOutputTokens}
	d.send(ctx, spec) // warm-up: not measured
	var ttfts []float64
	var tokens float64
	for range n {
		r := d.send(ctx, spec)
		if !r.rec.Completed() {
			if err := ctx.Err(); err != nil {
				return pt, err
			}
			return pt, fmt.Errorf("profile: prefill probe request (%d words) failed", words)
		}
		ttfts = append(ttfts, r.rec.TTFT().Seconds())
		tokens += float64(r.rec.InputTokens)
	}
	pt.Requests, pt.PromptTokens, pt.TTFTP50 = n, tokens/float64(n), eval.Percentile(ttfts, 50)
	return pt, nil
}

// level drives the replica closed-loop with c concurrent workers for
// warmup + window and measures the window (see measureLevel); requests
// still running at its end are aborted.
func (d *driver) level(ctx context.Context, c int, warmup, window time.Duration, lengths []workload.Spec, slo model.SLO) Level {
	start := time.Now()
	winStart, winEnd := start.Add(warmup), start.Add(warmup+window)
	return measureLevel(c, d.closedLoop(ctx, c, winEnd, lengths), winStart, winEnd, slo)
}

// closedLoop runs c workers that each send their next request as soon as
// the previous one ends, cycling through lengths, until end; requests
// still running at end are aborted. It returns every request.
func (d *driver) closedLoop(ctx context.Context, c int, end time.Time, lengths []workload.Spec) []sentRequest {
	lctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	var mu sync.Mutex
	var done []sentRequest
	var next atomic.Int64
	var wg sync.WaitGroup
	for range c {
		wg.Go(func() {
			for lctx.Err() == nil {
				r := d.send(lctx, lengths[int(next.Add(1)-1)%len(lengths)])
				mu.Lock()
				done = append(done, r)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return done
}

// measureLevel computes a level's statistics from its requests.
//
// Throughput counts the work of every request that completed inside
// [winStart, winEnd), whatever its arrival time. In a closed loop at steady
// state this is an unbiased estimate of the work rate (renewal-reward),
// whereas counting only requests that both arrived and completed inside the
// window would drop about one request per worker and window: the one in
// flight at winStart and the one aborted at winEnd.
//
// Latency percentiles use the requests that arrived and completed inside
// the window (their whole latency happened under the level's load), and
// failures the requests that arrived inside it and failed before its end.
func measureLevel(c int, reqs []sentRequest, winStart, winEnd time.Time, slo model.SLO) Level {
	lv := Level{Concurrency: c, FailedBy: map[string]int{}}
	var ttft, tpot []float64
	var out, weighted, promptTokens, words float64
	for _, r := range reqs {
		completed := r.rec.Completed()
		doneAt := r.sent.Add(r.rec.Done)
		inWindow := completed && !doneAt.Before(winStart) && doneAt.Before(winEnd)
		if inWindow {
			lv.Completed++
			out += float64(r.rec.OutputTokens)
			weighted += model.WeightedTokens(float64(r.spec.InputTokens), float64(r.rec.OutputTokens))
			promptTokens += float64(r.rec.InputTokens)
			words += float64(r.spec.InputTokens)
		}
		if r.arrivalAbs.Before(winStart) {
			continue
		}
		if !completed {
			if r.end.Before(winEnd) {
				lv.Failed++
				lv.FailedBy[r.failure]++
			}
			continue
		}
		if !inWindow {
			continue
		}
		lv.Requests++
		ttft = append(ttft, r.rec.TTFT().Seconds())
		if r.rec.OutputTokens > 1 {
			tpot = append(tpot, r.rec.TPOT().Seconds())
		}
	}
	secs := winEnd.Sub(winStart).Seconds()
	lv.TTFTP50, lv.TTFTP99 = eval.Percentile(ttft, 50), eval.Percentile(ttft, 99)
	lv.TPOTP50, lv.TPOTP99 = eval.Percentile(tpot, 50), eval.Percentile(tpot, 99)
	lv.OutputTokensPerSec, lv.WeightedTokensPerSec = out/secs, weighted/secs
	lv.PromptTokensPerWord = math.NaN()
	if words > 0 {
		lv.PromptTokensPerWord = promptTokens / words
	}
	lv.MeetsSLO = lv.Requests > 0 && lv.Failed == 0 &&
		lv.TTFTP99 <= slo.TTFTSeconds && lv.TPOTP99 <= slo.TPOT().Seconds()
	return lv
}
