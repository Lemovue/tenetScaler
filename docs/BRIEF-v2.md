# CONTEXT
Continue building TenantScaler in this repo. Sources of truth: PLAN.md (decisions Q1-Q8 and the
M1 measurements) and framework.txt. This prompt adds decisions Q9-Q18. Where they conflict
with those files, this prompt wins. FIRST, before writing any code, record Q9-Q18 in PLAN.md's
Decisions table and fix the stale lines listed under N1. Do not undo any M1-based conclusion.

# NEW DECISIONS
Q9  Cut the Kaggle/GPU tier. M1 measured flat aggregate throughput on one shared GPU
    (~630 tok/s for 1-6 replicas), and a T4 would repeat that. No tunnels anywhere.
Q10 The real headline set is S2, S3, S4, S5, S7 (S4 added: S4 vs S7 is the novelty test).
    The simulated appendix keeps S1-S7 plus S8.
Q11 Demand = OFFERED weighted tokens at gateway admission, including shed and queued requests.
    Output tokens = the request's max_tokens (lengths are forced, so this is exact).
    Add S8 = TenantScaler with naive demand (admitted/served tokens only), the death-spiral
    ablation. Write a unit test and a sim test that show S8 spiraling (shed -> lower measured
    demand -> scale-down -> more shed) BEFORE switching to offered demand, then show the
    offered-demand version does not spiral.
Q12 Real cold start (~0.6 s warm) hides the cold-start gap. Add a configurable readiness hold:
    the llama-server process starts and loads the model, but the pool marks the replica Ready
    only after T seconds. Label it everywhere as "injected load delay (emulates network model
    load); inference is real". Headline real runs at T=30 s AND T=0 (report both). Cold-start
    sweep 10/30/60/120 s: real if time allows, otherwise simulated and labelled.
Q13 CPU topology: the i5-14450HX is hybrid. Detect P- and E-cores (Windows
    GetLogicalProcessorInformationEx). Pin each replica to exactly one physical P-core
    (both of its threads). Pin the gateway, controller and local loadgen to the remaining cores.
    Re-run `profile -backend llama` after re-pinning; keep budget = 4 unless the new profile
    says otherwise. Log CPU frequency/temperature per run if readable; otherwise note that.
Q14 Experiment run order: for each run index, execute the setups in a seeded random order
    (interleaved, not grouped by setup). Report every individual run plus mean and 95% CI
    (t-distribution), and a drift check (first vs last run of each setup).
Q15 Pre-register the main claim in README.md before the first headline run:
    "During C's spike, at equal replica budget, S7 has higher victim (A, B) SLO attainment
    (TTFT p99 and TPOT both met) than S4, with C's shed % and shed-aware attainment reported
    alongside." Report the result whichever way it goes.
Q16 Multi-laptop live demo: add a `tenantscaler loadgen` subcommand (flags: gateway URL,
    API key, tenant profile steady|periodic|spike, --spike-now). Cross-compile for
    windows/amd64, darwin/arm64, linux/amd64 into dist/. Tenant identity and maximum priority
    come from the API key on the server side; a client header may only LOWER priority.
    Keep accepting x-llm-d-inference-fairness-id for llm-d compatibility, but in demo mode the
    API key wins. The gateway can bind 0.0.0.0. Add scripts/demo/firewall.ps1 (ask me before
    running it). The dashboard labels live demo traffic "live demo, not the experiment".
    Headline experiments use only the local, pinned loadgen.
Q17 Token counting: use llama-server's own tokenizer (its /tokenize endpoint or response usage
    fields) for input and output tokens. Remove whitespace counting from the real path; the
    simulated path may keep it, labelled as such.
Q18 Forecaster evaluation: fit and evaluate on a held-out, time-ordered segment of a real rate
    series (Azure LLM Inference Trace 2024 or BurstGPT, rescaled and cited) AND on synthetic
    tenant B. Compare against a seasonal-naive baseline. Report MAPE for both. Fit only on
    past windows (leakage check). Report its effect on B's reaction time and SLO attainment,
    even if the effect is null.
STRETCH ONLY: degrade-before-drop (clamp max_tokens for over-share sheddable traffic). It
    breaks identical work across setups, so it is a separate mode reported on its own, scored
    by useful tokens delivered, and never part of the headline results.

# MILESTONES (after each: guard -> build -> tests -> smoke run -> update PLAN.md -> commit)
N1 Reconcile the docs only: record Q9-Q18; fix R25, assumptions 2 and 3, the architecture
   diagram ("real llama-server replicas + labelled simulated engine"), the threats section
   (budget 4 real), and the Kaggle mentions. Requirements: promote R23 as Q15-K below, add
   S4/S8/readiness-hold/loadgen rows.
N2 Topology detection + pinning + re-profile. Write capacity C, TPOT p99 and cold start to
   config. Set SLOs: TTFT p99 <= 2 s; TPOT p99 bound derived from the profile, with the
   derivation written in DESIGN notes.
N3 Offered-demand estimator + S8 + the spiral tests (test first).
N4 Real harness: add S4 + readiness hold + interleaved runner + per-run CSV.
N5 loadgen subcommand + cross-compiled binaries + firewall script + DEMO.md (phone hotspot,
   not venue Wi-Fi; laptop plugged in, max-performance power plan; backup video steps).
N6 Headline real runs (5 setups x >=5 runs, T=30 and T=0), simulated appendix (S1-S8 +
   cold-start sweep), forecaster evaluation, results/real/summary.md, README numbers from
   measured results only.
N7 Q15-K Kubernetes live smoke: CHECKPOINT, ask me before installing WSL2 Ubuntu + k3s
   (CPU only, no NVIDIA). Demo: the controller scales a llama-server Deployment from 1 to 3 on
   a spike, TenantScaler.status updates, the gateway discovers replicas via EndpointSlices.
   Time-box 3 hours. Not used for headline numbers. If blocked, record exactly why in the README.
Cut order if time runs short: stretch mode, real cold-start sweep (keep simulated), N7, forecaster.
NEVER cut: S4 in real runs, S8, readiness hold, dispatch window, interleaved runs.

# RULES (in addition to PLAN.md's)
- Never invent a number, benchmark or citation. "Injected" and "simulated" labels appear in the
  UI, the README and every results table they apply to.
- Ask before: any system install, firewall change, or download other than the pinned model.
- No `go get` / `go mod tidy` while parallel agents are editing; use the heavy-step guard.
- Root-cause failures before patching. Keep CLAIMS.md mapping each pitch claim to the
  run/CSV that supports it.
Start with N1, and state any assumption you make.