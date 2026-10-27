package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/workload"
)

// liveProfiles maps a -profile name to the kind of tenant pattern it uses
// from the real scenario (A steady, B periodic, C spike).
var liveProfiles = map[string]string{
	"steady": workload.KindSteady, "periodic": workload.KindPeriodic, "spike": workload.KindSpike,
}

// loadgenLiveRequested reports whether the arguments ask for live mode.
func loadgenLiveRequested(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == "-gateway" || a == "--gateway" || strings.HasPrefix(a, "-gateway=") || strings.HasPrefix(a, "--gateway=")
	})
}

// runLoadgenLive sends ONE tenant's live traffic to a live-demo gateway
// (decision Q16): a teammate's laptop is one tenant, identified by its API
// key. It prints one line per window until Ctrl+C or -duration.
func runLoadgenLive(args []string) error {
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	gw := fs.String("gateway", "", "live-demo gateway base URL, e.g. http://192.168.43.10:8081")
	key := fs.String("key", os.Getenv("TS_API_KEY"), "this tenant's API key (default $TS_API_KEY); the gateway derives the tenant from it")
	profile := fs.String("profile", "steady", "traffic shape: steady | periodic | spike (the real scenario's A, B, C patterns)")
	spikeNow := fs.Bool("spike-now", false, "spike profile: start the spike immediately")
	spikeFor := fs.Duration("spike-duration", 40*time.Second, "spike profile: how long a spike lasts")
	duration := fs.Duration("duration", 0, "stop after this long (0 = until Ctrl+C)")
	every := fs.Duration("report-every", 10*time.Second, "print a summary line this often")
	seed := fs.Int64("seed", time.Now().UnixNano(), "seed of the arrivals and prompts (default: time-based)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	kind, ok := liveProfiles[*profile]
	switch {
	case *gw == "":
		return errors.New("-gateway is required")
	case *key == "":
		return errors.New("-key (or $TS_API_KEY) is required: the gateway decides the tenant from it")
	case !ok:
		return fmt.Errorf("-profile must be steady, periodic or spike, got %q", *profile)
	case *spikeNow && kind != workload.KindSpike:
		return errors.New("-spike-now needs -profile spike")
	}
	sc, err := liveScenario(kind)
	if err != nil {
		return err
	}
	var win liveWindow
	gen, err := loadgen.NewLive(sc, []loadgen.Target{{Name: "gateway", URL: *gw, APIKey: *key}}, *seed, loadgen.Options{
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		OnRecord: func(_ string, rec eval.Record) { win.add(rec) },
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	fmt.Printf("live demo traffic (not the experiment): profile %s (%s pattern) -> %s; Ctrl+C stops\n", *profile, sc.Tenants[0].Tenant, *gw)
	if err := gen.Start(ctx); err != nil {
		return err
	}
	if *spikeNow {
		gen.Spike(*spikeFor)
		fmt.Printf("spike started for %s\n", *spikeFor)
	}
	tick := time.NewTicker(*every)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			gen.Stop()
			gen.Wait()
			fmt.Println(win.line(time.Since(start), true))
			return nil
		case <-tick.C:
			fmt.Println(win.line(time.Since(start), false))
		}
	}
}

// liveScenario is the real scenario reduced to its tenant of the given
// kind.
func liveScenario(kind string) (workload.Scenario, error) {
	sc := workload.RealScenario()
	i := slices.IndexFunc(sc.Tenants, func(p workload.TenantPattern) bool { return p.Kind == kind })
	if i < 0 {
		return workload.Scenario{}, fmt.Errorf("the real scenario has no %s tenant", kind)
	}
	sc.Name, sc.Tenants = "live-"+kind, []workload.TenantPattern{sc.Tenants[i]}
	return sc, sc.Validate()
}

// liveWindow counts the outcomes since the last report and in total.
type liveWindow struct {
	mu                  sync.Mutex
	ok, shed, failed    int
	ttfts               []float64
	tOK, tShed, tFailed int
}

func (w *liveWindow) add(rec eval.Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case rec.Shed:
		w.shed++
		w.tShed++
	case rec.Failed:
		w.failed++
		w.tFailed++
	default:
		w.ok++
		w.tOK++
		w.ttfts = append(w.ttfts, rec.TTFT().Seconds())
	}
}

// line reports the window (and resets it) or, when total, the whole run.
func (w *liveWindow) line(elapsed time.Duration, total bool) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if total {
		return fmt.Sprintf("total after %s: %d answered, %d rejected with 429, %d failed", elapsed.Round(time.Second), w.tOK, w.tShed, w.tFailed)
	}
	p95 := "-"
	if len(w.ttfts) > 0 {
		p95 = fmt.Sprintf("%.2f s", eval.Percentile(w.ttfts, 95))
	}
	s := fmt.Sprintf("%6s  answered %3d  429 %3d  failed %3d  p95 time-to-first-token %s",
		elapsed.Round(time.Second), w.ok, w.shed, w.failed, p95)
	w.ok, w.shed, w.failed, w.ttfts = 0, 0, 0, w.ttfts[:0]
	return s
}
