package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tenantscaler/internal/experiment"
)

func init() {
	register("forecast-trace", "evaluate the demand forecaster on a REAL trace (Azure LLM inference 2024) and its SIMULATED effect on tenant B", runForecastTrace)
}

func runForecastTrace(args []string) error {
	fs := flag.NewFlagSet("forecast-trace", flag.ContinueOnError)
	trace := fs.String("trace", ".cache/traces/AzureLLMInferenceTrace_code_1week.csv", "Azure LLM inference trace CSV (TIMESTAMP, ContextTokens, GeneratedTokens)")
	cfgPath := fs.String("config", "config/real.yaml", "config of the simulated S7 vs S7F comparison (budget, C, calibrated engine)")
	bucket := fs.Duration("bucket", 5*time.Minute, "bucket of the rate series")
	season := fs.Int("season", 288, "season in buckets (288 x 5 min = one day)")
	heldOut := fs.Int("held-out", 3, "last whole seasons scored, one by one")
	runs := fs.Int("runs", 5, "simulation seeds for the effect on tenant B")
	seed := fs.Int64("seed", 1, "base seed")
	coldStart := fs.Duration("cold-start", 30*time.Second, "simulated replica cold start")
	out := fs.String("out", "results/forecast-real", "output directory for forecast-real.md")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	f, err := os.Open(*trace)
	if err != nil {
		return fmt.Errorf("open the trace (download it as PLAN.md decision Q18-A describes): %w", err)
	}
	series, err := experiment.LoadAzureTrace(f, *bucket)
	f.Close()
	if err != nil {
		return err
	}
	fmt.Printf("trace: %d requests from %s in %d buckets of %s\n", series.Rows, series.Start.Format(time.RFC3339), len(series.Weighted), *bucket)
	rep, err := experiment.EvaluateTrace(experiment.TracePlan{
		Series: series, Season: *season, HeldOutSeasons: *heldOut, Config: cfg, Runs: *runs, BaseSeed: *seed, ColdStart: *coldStart,
	})
	if err != nil {
		return err
	}
	md, err := experiment.TraceMarkdown(rep)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	path := filepath.Join(*out, "forecast-real.md")
	if err := os.WriteFile(path, md, 0o644); err != nil {
		return err
	}
	os.Stdout.Write(md)
	fmt.Printf("\nwrote %s\n", path)
	return nil
}
