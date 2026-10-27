package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"tenantscaler/internal/experiment"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

func init() {
	register("experiment", "run the SIMULATED evaluation (all setups x runs x cold starts) and the forecaster evaluation", runExperiment)
}

func runExperiment(args []string) error {
	fs := flag.NewFlagSet("experiment", flag.ContinueOnError)
	runs := fs.Int("runs", 5, "runs per setup and cold start (seeds seed .. seed+runs-1, the same for every setup)")
	coldStarts := fs.String("cold-starts", "10s,30s,60s,120s", "comma-separated replica cold-start sweep")
	defaultCS := fs.Duration("default-cold-start", 30*time.Second, "cold start of the main results table")
	setups := fs.String("setups", "all", "all (the specified setups plus the sensitivity variants), or a comma-separated list of: "+
		strings.Join(append(append([]string{}, sim.AllSetups...), sim.SensitivitySetups...), ", "))
	scenario := fs.String("scenario", "default", "workload scenario: default|reclaim|real|real-demo (the real-replica traces, for the simulated cross-check)")
	seed := fs.Int64("seed", 1, "base seed")
	out := fs.String("out", "results", "output directory")
	cfgPath := fs.String("config", "", "YAML config (empty = built-in default); its engine section sets the simulated engine")
	capacity := fs.Int("capacity", 0, "override pool.capacityReplicas to evaluate a binding budget (0 = the config's); tenant guarantees are scaled down to fit")
	initial := fs.Int("initial", 0, "warm replicas before traffic in every scaling setup (0 = the simulator default; the real runs use 2)")
	withForecast := fs.Bool("forecast", true, "also evaluate the demand forecaster (forecast.md); skipped when the scenario has no periodic tenant")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *capacity != 0 {
		if cfg, err = experiment.WithCapacity(cfg, *capacity); err != nil {
			return fmt.Errorf("-capacity: %w", err)
		}
	}
	sc, err := experiment.ScenarioByName(*scenario)
	if err != nil {
		return err
	}
	css, err := experiment.ParseDurations(*coldStarts)
	if err != nil {
		return fmt.Errorf("-cold-starts: %w", err)
	}
	var list []string
	if s := strings.TrimSpace(*setups); s != "" && s != "all" {
		for _, name := range strings.Split(s, ",") {
			list = append(list, strings.TrimSpace(name))
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	start := time.Now()
	sum, err := experiment.Run(experiment.Plan{
		Setups: list, Runs: *runs, ColdStarts: css, DefaultColdStart: *defaultCS,
		Scenario: sc, BaseSeed: *seed, InitialReplicas: *initial, OutDir: *out, Config: cfg, Logger: logger,
	})
	if err != nil {
		return err
	}
	fmt.Printf("SIMULATED evaluation, scenario %q, budget %d replicas, %d runs per setup, done in %v.\n\n",
		sc.Name, cfg.Pool.CapacityReplicas, sum.Runs, time.Since(start).Round(time.Millisecond))
	if err := expPrintMain(sum); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s/{runs.csv,summary.json,summary.md,*.svg}\n", *out)
	if !*withForecast {
		return nil
	}
	if !expHasPeriodic(sc) {
		fmt.Printf("forecaster evaluation skipped: scenario %q has no periodic tenant\n", sc.Name)
		return nil
	}
	if _, err := experiment.Forecast(experiment.ForecastPlan{
		Config: cfg, Scenario: sc, Runs: *runs, BaseSeed: *seed, ColdStart: *defaultCS, OutDir: *out,
	}); err != nil {
		return fmt.Errorf("forecaster evaluation: %w", err)
	}
	fmt.Printf("wrote %s/forecast.md\n", *out)
	return nil
}

// expPrintMain prints the headline metrics per setup at the default cold
// start, formatted like summary.md's main table.
func expPrintMain(sum experiment.Summary) error {
	t, err := experiment.MetricsTable(sum, sum.DefaultColdStartSeconds,
		"victim_violation_pct", "victim_shed_aware_violation_pct", "noisy_shed_pct", "replica_minutes")
	if err != nil {
		return err
	}
	fmt.Printf("cold start %gs (mean ± 95%% CI over runs):\n", sum.DefaultColdStartSeconds)
	return t.WriteMarkdown(os.Stdout)
}

func expHasPeriodic(sc workload.Scenario) bool {
	for _, t := range sc.Tenants {
		if t.Kind == workload.KindPeriodic {
			return true
		}
	}
	return false
}
