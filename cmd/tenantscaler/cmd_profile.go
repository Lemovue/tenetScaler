package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/experiment"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/realrun"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

func init() {
	register("profile", "measure the per-replica capacity C: REAL llama-server replica (backend llama) or the SIMULATED engine", runProfile)
}

func runProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "YAML config (empty = built-in default); its backend, engine section and tenant SLOs are profiled")
	backendKind := fs.String("backend", "", "simulated (virtual-time search on the simulated engine) or llama (one REAL replica); empty = backend.kind from the config")
	seed := fs.Int64("seed", 1, "base seed")
	// Simulated profile.
	duration := fs.Duration("duration", 300*time.Second, "simulated: simulated length of each probe")
	seeds := fs.Int("seeds", 3, "simulated: seeds per probe; a load meets the SLO only if every seed does")
	// Real profile.
	levelDur := fs.Duration("level-duration", realrun.DefaultLevelDuration, "llama: measured window per concurrency level")
	warmup := fs.Duration("warmup", realrun.DefaultWarmup, "llama: warm-up before each level's window")
	maxConc := fs.Int("max-concurrency", 0, "llama: highest concurrency level (0 = parallel slots + 2)")
	prefillN := fs.Int("prefill-requests", realrun.DefaultPrefillRequests, "llama: requests per prompt length of the prefill probe")
	checkDur := fs.Duration("check-duration", realrun.DefaultCheckLevelDuration, "llama: measured window per level of the calibration check on a SIMULATED replica (negative skips it)")
	out := fs.String("out", "results/real", "llama: directory for profile.md")
	logDir := fs.String("log-dir", llama.DefaultLogDir, "llama: directory for the replica log")
	scaling := fs.Int("scaling", 0, "llama: also measure the replica scaling curve for 1..N replicas loaded at once, and derive the SLO and C (0 = skip)")
	scalingOnly := fs.Bool("scaling-only", false, "llama: measure only the scaling curve (needs -scaling)")
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
	switch kind := cmp.Or(*backendKind, cfg.Backend.Kind); kind {
	case config.BackendSimulated:
		res, err := experiment.Profile(cfg, experiment.ProfileOptions{Duration: *duration, Seeds: *seeds, BaseSeed: *seed})
		if err != nil {
			return err
		}
		return res.WriteText(os.Stdout)
	case config.BackendLlama:
		cfg.Backend.Kind = config.BackendLlama
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		in, outLen := workload.RealLengths()
		plan := realrun.ProfilePlan{
			Config: cfg, In: in, Out: outLen, MaxConcurrency: *maxConc, LevelDuration: *levelDur, Warmup: *warmup,
			PrefillRequests: *prefillN, CheckLevelDuration: *checkDur, Seed: *seed, LogDir: *logDir,
		}
		if *scalingOnly && *scaling == 0 {
			return errors.New("-scaling-only needs -scaling N")
		}
		if !*scalingOnly {
			if err := runRealProfile(cfg, plan, *out); err != nil {
				return err
			}
		}
		if *scaling > 0 {
			return runScalingCurve(cfg, plan, *scaling, *out)
		}
		return nil
	default:
		return fmt.Errorf("-backend must be %q or %q, got %q", config.BackendSimulated, config.BackendLlama, kind)
	}
}

// runRealProfile measures one real replica, prints the result and writes
// profile.md. Ctrl-C stops it and kills the replica.
func runRealProfile(cfg config.Config, plan realrun.ProfilePlan, outDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plan.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	fmt.Printf("profiling one REAL llama-server replica (%d threads, %d slots), up to concurrency %d...\n",
		cfg.Backend.Llama.Threads, cfg.Backend.Llama.Parallel, cmp.Or(plan.MaxConcurrency, cfg.Backend.Llama.Parallel+2))
	res, err := realrun.Profile(ctx, plan)
	if err != nil {
		return err
	}
	if err := res.WriteText(os.Stdout); err != nil {
		return err
	}
	if err := res.Validate(); err != nil {
		fmt.Printf("warning: the calibrated engine parameters are not a valid simulated engine: %v\n", err)
	}
	path, err := res.WriteFiles(outDir)
	if err != nil {
		return err
	}
	fmt.Printf("\nwrote %s\n", path)
	return nil
}

// runScalingCurve measures the replica scaling curve, prints it with the
// SLO and capacity derivation, and writes scaling.md and scaling.csv.
func runScalingCurve(cfg config.Config, plan realrun.ProfilePlan, n int, outDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plan.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	fmt.Printf("\nREAL replica scaling curve: 1..%d llama-server replicas loaded at once, %d concurrent requests each...\n",
		n, stack.BackendSlots(cfg))
	levels, err := realrun.ScalingCurve(ctx, plan, n, 0)
	if err != nil {
		return err
	}
	var tb strings.Builder
	if err := realrun.ScalingTable(levels).WriteMarkdown(&tb); err != nil {
		return err
	}
	table := tb.String()
	fmt.Print(table)
	der, err := realrun.DeriveBudget(levels, n)
	if err != nil {
		return err
	}
	fmt.Print("\n" + der.Explain())
	if err := realrun.WriteScalingCSV(filepath.Join(outDir, "scaling.csv"), levels); err != nil {
		return err
	}
	md := fmt.Sprintf("# Replica scaling curve\n\n**REAL inference: %s, %s**\n\n"+
		"k replicas loaded at once, each closed-loop at %d concurrent requests (the dispatch window), unique prompts, "+
		"output length forced to max_tokens; %s warm-up, %s measured per level. Throughput is the sum over replicas, "+
		"percentiles are pooled. Per-replica rows: scaling.csv.\n\n%s\n## SLO and capacity derivation (rules: docs/DESIGN.md section 20)\n\n%s",
		realrun.CPUModel(), time.Now().Format("2006-01-02"), stack.BackendSlots(cfg), plan.Warmup, plan.LevelDuration,
		table, der.Explain())
	path := filepath.Join(outDir, "scaling.md")
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nwrote %s and scaling.csv\n", path)
	return nil
}
