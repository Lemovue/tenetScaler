package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/realrun"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

func init() {
	register("realrun", "run the evaluation on REAL llama-server replicas (setups one at a time, mean ± 95% CI over runs)", runRealrun)
}

// defaultRealSetups are the headline setups of decisions Q6 and Q10.
var defaultRealSetups = []string{sim.SetupKEDAQueue, sim.SetupStaticPartition, sim.SetupFlowControl, sim.SetupNoGateway, sim.SetupFull}

func runRealrun(args []string) error {
	fs := flag.NewFlagSet("realrun", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/real.yaml", "YAML config; backend.kind llama runs real replicas (simulated runs are labelled SIMULATED)")
	setups := fs.String("setups", strings.Join(defaultRealSetups, ","), "comma-separated setups, from: "+strings.Join(stack.Supported, ", "))
	runs := fs.Int("runs", 5, "runs per setup (run k uses seed+k for every setup)")
	initial := fs.Int("initial", realrun.DefaultInitialReplicas, "replicas started and ready before traffic (the static partition uses the whole budget)")
	scenario := fs.String("scenario", "real", "workload: real (about 4 min) | real-demo (about 2 min)")
	out := fs.String("out", "results/real", "output directory")
	overwrite := fs.Bool("overwrite", false, "remove an earlier run's results from the output directory first (other files, like profile.md, are kept)")
	seed := fs.Int64("seed", 1, "base seed")
	cooldown := fs.Duration("cooldown", 20*time.Second, "pause between runs")
	drain := fs.Duration("drain-timeout", realrun.DefaultDrainTimeout, "longest wait for in-flight requests after the scenario ends")
	logDir := fs.String("log-dir", llama.DefaultLogDir, "directory for the llama-server logs")
	hold := fs.Duration("hold", 0, "readiness hold T: a launched replica counts as ready only T after launch ("+pool.HoldLabel+"); the headline runs use 30s and 0")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	sc, err := realScenario(*scenario)
	if err != nil {
		return err
	}
	list := ctlSplitList(*setups)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	perRun := sc.Duration + *cooldown + *hold
	fmt.Printf("realrun: %d setups x %d runs of scenario %q (%s each, backend %s), at least %s in total; results in %s\n",
		len(list), *runs, sc.Name, sc.Duration, cfg.Backend.Kind,
		(time.Duration(len(list)**runs) * perRun).Round(time.Minute), *out)
	sum, err := realrun.Run(ctx, realrun.Plan{
		Config: cfg, Setups: list, Runs: *runs, Scenario: sc, InitialReplicas: *initial, OutDir: *out,
		BaseSeed: *seed, Cooldown: *cooldown, LogDir: *logDir, DrainTimeout: *drain, Overwrite: *overwrite, Logger: logger,
		ReadinessHold: *hold,
	})
	if len(sum.Results) > 0 {
		fmt.Printf("\n%s\n\n", sum.Label)
		if perr := printRealSummary(sum, sc); perr != nil {
			err = errors.Join(err, perr)
		}
		fmt.Printf("\nwrote %s/{summary.md,summary.json,runs.csv,*.svg,records/,timeline/,decisions/}\n", *out)
	}
	return err
}

// realScenario maps the -scenario flag to a workload.
func realScenario(name string) (workload.Scenario, error) {
	switch name {
	case "real":
		return workload.RealScenario(), nil
	case "real-demo":
		return workload.RealDemoScenario(), nil
	}
	return workload.Scenario{}, fmt.Errorf("-scenario must be real or real-demo, got %q", name)
}

// printRealSummary prints the headline metrics per setup.
func printRealSummary(sum realrun.Summary, sc workload.Scenario) error {
	want := map[string]bool{"victim_violation_pct": true, "victim_shed_aware_violation_pct": true,
		"noisy_shed_pct": true, "reaction_time_s": true, "replica_minutes": true, "jain_index": true}
	var tenants []string
	for _, t := range sc.Tenants {
		tenants = append(tenants, t.Tenant)
		want["p99_ttft_s_"+t.Tenant] = true
	}
	var metrics []eval.Metric
	for _, m := range eval.Metrics(tenants) {
		if want[m.Name] {
			metrics = append(metrics, m)
		}
	}
	fmt.Println("mean ± 95% CI over runs (all metrics in summary.md):")
	return realrun.MainTable(sum, metrics).WriteMarkdown(os.Stdout)
}
