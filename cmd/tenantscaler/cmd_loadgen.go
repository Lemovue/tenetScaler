package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

func init() {
	register("loadgen", "replay a scenario to OpenAI-compatible targets, or (-gateway) send one tenant's live traffic to a live-demo gateway", runLoadgen)
}

func runLoadgen(args []string) error {
	if loadgenLiveRequested(args) {
		return runLoadgenLive(args)
	}
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	targetsFlag := fs.String("targets", "", "comma-separated name=url targets; each receives every request "+
		"(e.g. baseline=http://127.0.0.1:8080,tenantscaler=http://127.0.0.1:8081)")
	scenarioName := fs.String("scenario", "default", "workload scenario: default|reclaim")
	seed := fs.Int64("seed", 1, "seed of the trace (arrivals, priorities, lengths)")
	promptSeed := fs.Int64("prompt-seed", 0, "seed of the prompt words (default: time-based, printed so a run can be repeated); "+
		"give every run against the same long-lived llama-server replicas its own value, so no run is served from an earlier run's prompt cache")
	speed := fs.Float64("speed", 1, "time compression (2 = twice as fast); measured latencies are never scaled")
	duration := fs.Duration("duration", 0, "replay only the first part of the scenario (0 = all of it)")
	maxInFlight := fs.Int("max-inflight", 1024, "max concurrent requests per target (0 = no cap); requests over it are recorded as failed")
	out := fs.String("out", "records.csv", "CSV file for the per-request records (empty = do not write)")
	cfgPath := fs.String("config", "", "YAML config whose tenant SLOs are used in the summary (empty = built-in default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	targets, err := loadgenTargets(*targetsFlag)
	if err != nil {
		return err
	}
	sc, err := loadgenScenario(*scenarioName)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *speed <= 0 || math.IsInf(*speed, 0) || math.IsNaN(*speed) {
		return fmt.Errorf("-speed must be a finite number > 0, got %v", *speed)
	}
	specs := workload.Generate(sc, *seed)
	if *duration > 0 {
		n := sort.Search(len(specs), func(i int) bool { return specs[i].Offset >= *duration })
		specs = specs[:n]
	}
	if len(specs) == 0 {
		return errors.New("the scenario produced no requests in the chosen duration")
	}
	if !loadgenFlagGiven(fs, "prompt-seed") {
		*promptSeed = time.Now().UnixNano()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	last := specs[len(specs)-1].Offset
	fmt.Printf("loadgen: scenario %q seed %d prompt-seed %d: %d requests over %s at speed %g (about %s wall clock) to %d target(s)\n",
		sc.Name, *seed, *promptSeed, len(specs), last.Round(time.Second), *speed,
		time.Duration(float64(last) / *speed).Round(time.Second), len(targets))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	res, runErr := loadgen.ReplayResult(ctx, specs, targets, loadgen.Options{
		Speed: *speed, MaxInFlight: *maxInFlight, Seed: *promptSeed, Logger: logger,
	})
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	if *out != "" {
		if err := loadgenWriteRecords(*out, res); err != nil {
			return err
		}
		fmt.Printf("records written to %s\n", *out)
	}
	slos := map[string]model.SLO{}
	for _, t := range cfg.Tenants {
		slos[t.Name] = t.SLO
	}
	if err := loadgenPrintSummary(os.Stdout, res, slos); err != nil {
		return err
	}
	loadgenPrintWindows(sc, *speed)
	if runErr != nil {
		return fmt.Errorf("interrupted: only the requests sent so far were recorded: %w", runErr)
	}
	return nil
}

// loadgenTargets parses "name=url,name=url".
func loadgenTargets(s string) ([]loadgen.Target, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("-targets is required (name=url,...)")
	}
	var out []loadgen.Target
	for _, part := range strings.Split(s, ",") {
		name, url, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("-targets: %q is not name=url", part)
		}
		out = append(out, loadgen.Target{Name: name, URL: url})
	}
	return out, nil
}

// loadgenScenario returns a built-in scenario.
func loadgenScenario(name string) (workload.Scenario, error) {
	switch name {
	case "default":
		return workload.DefaultScenario(), nil
	case "reclaim":
		return workload.ReclaimScenario(), nil
	}
	return workload.Scenario{}, fmt.Errorf("-scenario must be default or reclaim, got %q", name)
}

// loadgenFlagGiven reports whether the named flag was given on the command line.
func loadgenFlagGiven(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// loadgenWriteRecords writes the records CSV to path.
func loadgenWriteRecords(path string, res loadgen.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := loadgen.WriteRecordsCSV(f, res); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// loadgenPrintSummary prints, per target and tenant, outcome counts, TTFT
// p50/p99, TPOT p99 and the share of requests meeting the SLO, both over
// non-shed requests and with shed requests counted as misses (so a stack
// that sheds its hardest requests does not look better for it). Targets
// whose responses came from a simulated replica are marked SIMULATED.
func loadgenPrintSummary(w io.Writer, res loadgen.Result, slos map[string]model.SLO) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "target\ttenant\trequests\tcompleted\tshed\tfailed\tTTFT p50 s\tTTFT p99 s\tTPOT p99 ms\t"+
		"SLO met % (excl. shed)\tSLO met % (shed = miss)")
	names := make([]string, 0, len(res.Records))
	for n := range res.Records {
		names = append(names, n)
	}
	sort.Strings(names)
	var simulated []string
	for _, n := range names {
		label := n
		if res.Simulated(n) {
			label += " [SIMULATED]"
			simulated = append(simulated, n)
		}
		byTenant := map[string][]eval.Record{}
		for _, r := range res.Records[n] {
			byTenant[r.Tenant] = append(byTenant[r.Tenant], r)
		}
		tenants := make([]string, 0, len(byTenant))
		for t := range byTenant {
			tenants = append(tenants, t)
		}
		sort.Strings(tenants)
		for _, t := range tenants {
			var ttft, tpot []float64
			completed, shed, failed, met := 0, 0, 0, 0
			slo, hasSLO := slos[t]
			for _, r := range byTenant[t] {
				switch {
				case r.Shed:
					shed++
					continue
				case r.Completed():
					completed++
					ttft = append(ttft, r.TTFT().Seconds())
					if r.OutputTokens > 1 {
						tpot = append(tpot, r.TPOT().Seconds()*1000)
					}
				default:
					failed++
				}
				if hasSLO && r.MetSLO(slo) {
					met++
				}
			}
			metPct, metShedPct := math.NaN(), math.NaN()
			if hasSLO && completed+failed > 0 {
				metPct = 100 * float64(met) / float64(completed+failed)
			}
			if hasSLO {
				metShedPct = 100 * float64(met) / float64(len(byTenant[t]))
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", label, t, len(byTenant[t]), completed, shed, failed,
				eval.FormatValue(eval.Percentile(ttft, 50)), eval.FormatValue(eval.Percentile(ttft, 99)),
				eval.FormatValue(eval.Percentile(tpot, 99)), eval.FormatValue(metPct), eval.FormatValue(metShedPct))
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(simulated) > 0 {
		_, err := fmt.Fprintf(w, "SIMULATED: %s answered from simulated replicas (header X-TenantScaler-Simulated: true); "+
			"their latencies are modelled by the simulated engine, not measured on real inference\n", strings.Join(simulated, ", "))
		return err
	}
	return nil
}

// loadgenPrintWindows prints the scenario's windows in record time: the
// records hold wall-clock offsets, so windows are divided by speed.
func loadgenPrintWindows(sc workload.Scenario, speed float64) {
	div := func(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()/speed) }
	for _, p := range sc.Tenants {
		if start, end, ok := sc.SpikeWindow(p.Tenant); ok {
			fmt.Printf("tenant %s spike window in record time (scenario time / speed): [%s, %s)\n", p.Tenant, div(start), div(end))
		}
	}
}
