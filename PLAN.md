# TenantScaler — Build Plan

## Machine resources (detected, Section 1.1)

| Item | Detected |
|---|---|
| OS | Windows 11 Home Single Language 10.0.26300 |
| CPU | Intel Core i5-14450HX — 10 cores / 16 logical |
| RAM | 16,088 MB total, 7,991 MB free (50.6% used at start) |
| GPU | NVIDIA GeForce RTX 3050 6GB Laptop GPU — 0 / 6,144 MiB used, 31 °C |
| Free disk | 160 GB on C: |
| Toolchain | Go 1.27.0, Python 3.12.10 (psutil, numpy), Node 24.20.0, Git 2.54 |
| Missing | Docker, kind, kubectl, helm, Ollama, make, golangci-lint |

**Derived settings**

| Setting | Value | Why |
|---|---|---|
| N (parallel jobs) | 8 | half of 16 logical cores |
| Go build/test parallelism | `-p 4`, `GOMAXPROCS=8` | RAM, not CPU, is the binding limit (client-go compiles are memory-heavy) |
| Node heap | `--max-old-space-size=4000` | ~25% of 16 GB (Node is only used for `node --check`) |
| ML batch size | 1 (no neural model is loaded) | the ML part is a statistical forecaster on CPU |
| Heavy steps | one at a time via `scripts/dev/heavy.py` (file lock + guard) | Section 1.3 |

Guard: `python scripts/dev/resource_guard.py` (exit 1 on any hard stop).

---

## Problem (3–4 sentences)

Several teams share one LLM served by one fixed pool of replicas, each promised a latency SLO. When one team spikes, the others miss their SLO although their traffic did not change (noisy neighbour). Today's per-workload autoscalers (HPA/KEDA) cannot decide who gets scarce replicas, and even a correct decision arrives late because a new replica needs time to load (the cold-start gap). TenantScaler joins a budget-aware arbiter (guaranteed shares, borrowing, reclaim) to a tenant-fair gateway that enforces the same decision at request level within one control cycle, and explains every decision.

**Target user:** a platform/SRE team running one shared LLM for several internal teams or customers on a fixed replica budget (GPUs in production; on this laptop, real llama.cpp replicas each pinned to one physical P-core).

**Core user flow (demo):** operator opens the dashboard → **Run A/B** replays the same recorded traffic (A steady, B periodic, C spiky) first on the queue-based KEDA baseline, then on TenantScaler, each on the full budget of real llama.cpp replicas → baseline victims A and B miss their SLO during C's spike; TenantScaler keeps them within SLO by re-weighting the gateway, reserving slots and shedding C's sheddable traffic while replicas cold-start → operator reads the decision log → operator opens the results (real: S2, S3, S4, S5, S7 × ≥5 interleaved runs at readiness hold T = 30 s and T = 0, mean ± 95% CI; simulated appendix S1–S8, labelled). Live variant (Q16): teammates' laptops send tenant traffic with `tenantscaler loadgen` and their own API keys; the dashboard labels it "live demo, not the experiment".

---

## Requirements matrix

Sources: `TenantScaler_Final/TenantScaler_Problem_Statement.pdf` (PS), `framework.txt` (FW), user decisions (UD).
Status values: Planned → In progress → **Done** / Blocked / Cut.

### Must-have

| ID | Requirement (source) | Component | Verified by | Status |
|---|---|---|---|---|
| R1 | Allocate scarce replicas: guaranteed shares, borrowing of idle capacity, reclaim when owner returns; worked examples 2/4/4 and 3/4/3 (PS §2, §5) | `internal/arbiter` | unit + property tests | Planned |
| R2 | Reject configs whose guarantees exceed capacity (PS §5 Mayank) | `internal/config`, `internal/arbiter` | unit tests | Planned |
| R3 | Control cycle: read per-tenant token rate, queue, SLO attainment → estimate demand (token formula) → arbitrate → stabilize (up fast, down slow, max step) → act (pool size) → push share/shedding/retry-after to gateway → explain (PS §5) | `internal/controller`, `internal/estimate`, `internal/stabilize` | unit tests + e2e | Planned |
| R4 | Fail-static: missing metrics ⇒ change nothing (PS §5) | `internal/controller` | unit test + e2e | Planned |
| R5 | Gateway: per-tenant queues, token-fair counter with lift (VTC), priority classes critical/standard/sheddable, HTTP 429 + Retry-After for over-share sheddable traffic, policy intake within one cycle (PS §2, §5 Harshit) | `internal/fairq`, `internal/gateway` | unit tests + e2e | Planned |
| R6 | Every allocation decision explained in a log shown on the dashboard (PS §2) | `internal/controller`, `web/` | e2e (`/api/decisions`) | Planned |
| R7 | Kubernetes controller + `TenantScaler` CRD: reads CRs, scales the pool Deployment, writes status (PS §2) | `internal/k8s`, `deploy/k8s/` | unit tests with client-go fakes + manifest validation test | Planned |
| R8 | Replay the same trace (A steady, B periodic, C spike) against 5 setups with the same model/cluster/budget, ≥5 runs each, mean ± 95% CI (PS §3) | `internal/sim`, `internal/eval`, `internal/experiment` | `tenantscaler experiment` output + `results/summary.md` | Planned |
| R9 | Metrics: victim SLO violation % during spike (main), p99 per tenant, shed % noisy tenant, reaction time, replica-minutes, Jain's index (PS §3) | `internal/eval` | unit tests + results | Planned |
| R10 | Dashboard with two-panel KEDA vs TenantScaler view and Spike C (PS §5 step 5) | `internal/demo`, `web/` | e2e smoke test (API the UI uses) + manual | Planned |
| R11 | Reproduce the problem first: baseline victims break when C spikes (PS §5 step 1) | experiment | results table | Planned |
| R12 | Honest labelling: anything simulated is labelled in UI and README; no unmeasured numbers (PS §4) | `web/`, README | review + UI badge | Planned |

### Should-have

| ID | Requirement (source) | Component | Verified by | Status |
|---|---|---|---|---|
| R13 | Extra setups: Flow-Control-equivalent baseline (S4) and no-borrowing ablation (S6) (FW 4.1); S4 is also a real headline setup (R30) | `internal/sim`, `internal/fairq`, `internal/stack` | results | Planned |
| R14 | TTFT + TPOT SLOs; shed-aware attainment; reclaim latency; policy propagation latency; max service gap (FW 4.3) | `internal/eval`, gateway, controller | unit tests + results | Planned |
| R15 | Dispatch window, EDF within tenant, critical-quota anti-gaming, jittered Retry-After (FW 3.3) | `internal/fairq`, `internal/gateway` | unit tests | Planned |
| R16 | Cold-start sweep 10/30/60/120 s (FW 4.2): real with the readiness hold if time allows, otherwise simulated and labelled (Q12; cut order item 2) | experiment, `realrun` | results | Planned |
| R17 | ML: Holt-Winters per-tenant demand forecaster with reactive fallback vs a seasonal-naive baseline; MAPE on a held-out, time-ordered segment of a REAL rate series (Azure LLM Inference Trace 2024 or BurstGPT, rescaled and cited; download needs approval) AND on synthetic tenant B; fit on past windows only (leakage check); effect on B's reaction time and SLO attainment reported even if null; failure analysis (UD, FW U5, Q18; cut order item 4) | `internal/forecast`, experiment | unit tests + results | Planned (synthetic part done in `results/sim/*/forecast.md`) |
| R18 | Prometheus `/metrics` on gateway + replicas; Prometheus metrics source for the controller (PS §5) | gateway, replica, controller | unit tests | Planned |
| R19 | OpenAI-compatible upstream adapter (static endpoints, optional API key from `.env`) (UD) | `internal/gateway` | e2e against simulated replica | Planned |
| R20 | Live-vs-simulation cross-check run (UD) | experiment | results | Planned |
| R21 | K8s manifests incl. KEDA/HPA baseline manifests + Dockerfile (FW) | `deploy/` | manifest test | Planned |
| R22 | `profile` command to measure per-replica capacity C (FW 3.2) | `internal/experiment` | command output | Planned |

### Must-have (added by decisions U1, Q5–Q8: real metrics)

| ID | Requirement (source) | Component | Verified by | Status |
|---|---|---|---|---|
| R26 | Real inference replicas: pool launches real `llama-server` processes (Qwen2.5-0.5B), CPU-pinned, real cold start, real streaming; killed on stop (no orphans) (U1, Q5, Q8) | `internal/llama`, `internal/pool` backends | unit tests + real e2e test + process check | Planned |
| R27 | Headline metrics measured on REAL replicas: S2 KEDA, S3 static, S4 Flow-Control-equivalent, S5 no-gateway, S7 TenantScaler × ≥5 runs, at readiness hold T = 30 s and T = 0, every run plus mean ± 95% CI (t) (Q6, Q10, Q12, Q14) | `internal/realrun`, `tenantscaler realrun` | command output + `results/real/summary.md` | Planned |
| R28 | Real profile measures per-replica capacity C and calibrates the simulated engine (Q6, Q8) | `tenantscaler profile -backend llama` | command output | Planned |
| R29 | Demo runs the same traffic sequentially on baseline then TenantScaler on real replicas; falls back to the simulated engine (labelled) if llama.cpp is not installed (Q7) | `internal/demo`, `web/` | e2e smoke test + manual | Planned |

### Must-have (added by BRIEF-v2, decisions Q9–Q18)

| ID | Requirement (source) | Component | Verified by | Status |
|---|---|---|---|---|
| R30 | S4 (Flow-Control-equivalent: roundrobin gateway + KEDA on gateway queue) runs on REAL replicas in the headline set; S4 vs S7 is the novelty test (Q10; never cut) | `internal/stack`, `internal/realrun` | N4 smoke: `.run/n4-smoke/summary.md` (all 5 setups real) | Built (N4); headline runs in N6 |
| R31 | S8 = TenantScaler with naive demand (admitted/served tokens only), the death-spiral ablation; a unit test and a sim test, written before the S8 wiring, show S8 spiralling (shed → lower measured demand → scale-down → more shed) and the offered-demand estimator not spiralling (Q11; never cut) | `internal/control` (`NaiveDemand`), `internal/gateway` + `internal/sim` (admitted rate), `internal/sim` (S8) | `TestNaiveDemandSpirals`, `TestNaiveDemandDoesNotSpiralInSimulation` (tests first: c3d68df) | Done (N3); the spiral reproduces in the control core but not in the full system, see Q11-R |
| R32 | Readiness hold T: the llama-server process starts and loads the model, the pool marks it Ready only after T seconds; labelled "injected load delay (emulates network model load); inference is real" in the UI, README and every table (Q12; never cut) | `pool.WithReadinessHold`, `realrun -hold` | `TestReadinessHold`; N4 smoke (every cold start 30.000 s, label present) | Done (N4) |
| R33 | CPU topology: detect P- and E-cores (Windows `GetLogicalProcessorInformationEx`); pin each replica to exactly one physical P-core (both threads); pin gateway, controller and local loadgen to the remaining cores; re-profile; log CPU frequency/temperature per run if readable, otherwise say so (Q13) | `internal/topology`, `internal/llama` (pinning), `internal/realrun` | `TestDetectThisMachine`, `TestLaunchPinsReplicaProcess`, `results/real/scaling.md` | Done for profiles (N2); per-run frequency log in N4 |
| R34 | Seeded interleaved run order per run index; report every run, mean and 95% CI (t-distribution) and a drift check (first vs last run of each setup); per-run CSV (Q14; never cut) | `internal/realrun` (`runOrder`, runs table, drift table, per-run facts in summary.md and summary.json) | `TestRunOrder`, `TestRunEndToEndSimulated`; N4 smoke | Done (N4) |
| R35 | Main claim pre-registered in README before the first headline run (Q15 text) and reported whichever way it goes | README "Pre-registered main claim"; `realrun` computes it (`claim.go`) | git history; `TestBuildClaim` | Done (pre-registered before N6) |
| R36 | Live multi-laptop demo: `tenantscaler loadgen` (gateway URL, API key, profile steady/periodic/spike, `--spike-now`), cross-compiled for windows/amd64, darwin/arm64, linux/amd64 into `dist/`; tenant identity and maximum priority from the API key (a header may only lower priority); `x-llm-d-inference-fairness-id` still accepted, the key wins in demo mode; gateway may bind 0.0.0.0; `scripts/demo/firewall.ps1` (run only with approval); dashboard label "live demo, not the experiment"; headline runs use only the local pinned loadgen (Q16) | `gateway` (KeyGrant, PublicHandler), `stack.Options.PublicAddr`, `demo -live`, `loadgen -gateway`, `scripts/build-dist.*`, `scripts/demo/firewall.ps1`, `DEMO.md` | `TestTenantAndAdminKeys`, `TestPublicHandler`, `TestLiveRun`; live run with three loadgen processes (A, B: 0 × 429; C: 67 × 429); `dist/` built | Done (N5); firewall script not run (needs approval) |
| R37 | Real-path token counting with llama-server's tokenizer (`/tokenize` or response `usage`) for input and output tokens; output length forced to `max_tokens` so offered demand is exact; whitespace counting only on the simulated path, labelled (Q11, Q17) | `gateway.inputTokens`, `llama.TokenCounter` | `TestTokenCounter` (gateway, llama); N4 smoke: 0 tokenizer fallbacks on real replicas | Done (N4) |
| R38 | `CLAIMS.md` maps every pitch claim to the run/CSV that supports it (BRIEF-v2 rules) | `CLAIMS.md` | review | In progress (headline rows after N6) |

### Stretch (BRIEF-v2; first in the cut order)

| ID | Requirement | Status |
|---|---|---|
| R39 | Degrade-before-drop: clamp `max_tokens` of over-share sheddable traffic; a separate mode scored by useful tokens delivered, never part of the headline results | Planned (stretch) |

### Nice-to-have

| ID | Requirement | Status |
|---|---|---|
| R23 → Q15-K | Kubernetes live smoke (N7): WSL2 Ubuntu + k3s, CPU only; the controller scales a llama-server Deployment 1 → 3 on a spike, `TenantScaler.status` updates, the gateway discovers replicas via EndpointSlices. Checkpoint: ask before installing. Time-box 3 h; not used for headline numbers; if blocked, the exact reason goes in the README (cut order item 3) | **Done 2026-10-28** with the simulated pool instead of llama-server (approved install of WSL2 + k3s v1.36.5; image built without Docker): pool 1 → 8 → 1, status and EndpointSlices verified, c shed with the demo sizing. `results/k8s/`, `deploy/k3s/README.md` |
| R24 | "What-if" latency estimate (PS: cut first) | Cut |
| R25 | Real-model inference run | Superseded by R26–R29 (decision U1): real llama.cpp inference is the headline path |

---

## Judging criteria

No official rubric was found in the folder (only team-written docs). Mapping against the generic criteria used in `framework.txt`:

| Criterion | Evidence we will produce |
|---|---|
| Innovation | per-tenant shares inside one pool + arbiter-driven gateway weights; S4 (Flow-Control-equivalent) vs S7 comparison; reclaim latency |
| Technical depth | arbiter invariants (property tests), VTC with lift, dispatch window, K8s controller, DES |
| Feasibility / working demo | one-command local demo, e2e smoke test, fresh-clone setup proof |
| Evaluation rigour | real: 5 setups × ≥5 seeded interleaved runs × 2 readiness holds, every run + t-based 95% CI + drift check, pre-registered claim; simulated appendix S1–S8 + cold-start sweep (labelled); threats to validity |
| Impact | operator dashboard + decision provenance |
| Presentation | `DEMO.md` 3-minute script, labelled simulation |

---

## Architecture and stack

```
tenants ──HTTP──▶ gateway (fairq) ──dispatch window──▶ replica pool: real llama-server replicas (1 per P-core)
                                                        | labelled simulated engine (simulator, fallback)
                    ▲  │ /stats, /metrics                         ▲ scale
            policy  │  ▼                                          │
                 controller (estimate → forecast → arbiter → stabilize) ──▶ actuator (local pool | K8s Deployment)
                    └──▶ decision log ──▶ dashboard
evaluation: internal/sim runs the SAME fairq + controller core in virtual time
```

| Choice | Why | Rejected alternative |
|---|---|---|
| Go for all services (one binary, subcommands) | K8s-native, fast HTTP, the user knows Go; one binary keeps setup to `go build` | Python services (slower gateway, two runtimes) |
| Python only for `resource_guard.py` / `heavy.py` | brief mandates stdlib + psutil | — |
| client-go (dynamic client for CRs, typed scale/EndpointSlice) with a periodic level-triggered reconcile | no codegen tools available (controller-gen/Kubebuilder CLI not installed); a 10–15 s periodic loop needs no informer cache | controller-runtime/Kubebuilder (needs codegen + more deps) |
| Real replicas: llama.cpp `llama-server` (b11898) with Qwen2.5-0.5B-Instruct Q4_K_M, one per physical P-core (Q8, Q13); simulated engine (llm-d-inference-sim-style parameters, calibrated to the real profile) only for the labelled appendix and the no-llama fallback | real inference for the headline (U1); the GPU tier is cut (Q9: M1 measured flat aggregate throughput on one shared GPU) | GPU replicas (M1), Kaggle/cloud GPU (Q9), llm-d-inference-sim itself (needs Docker/ZMQ build) |
| Discrete-event simulation in virtual time reusing the real `fairq` and controller core | 7 setups × ≥5 runs + cold-start sweep in minutes (UD) | real-time replay only (~70+ min, noisier) |
| Vanilla HTML/CSS/JS dashboard embedded in the binary (`go:embed`), charts drawn on canvas | no npm build, works offline, nothing to install | React + bundler (extra toolchain, slower setup) |
| `client_golang` for `/metrics`, `sigs.k8s.io/yaml` for config | standard, already in the client-go dependency tree | hand-written exposition format |
| Holt-Winters (additive) forecaster in pure Go | cheap, explainable, fits the periodic tenant (UD) | ARIMA (needs a fitting library), neural model (GPU, data) |

---

## Assumptions

1. One model shared by all tenants (PS §4). Tenants on different models and per-tenant adapters are out of scope.
2. Tokens (Q11, Q17): on the real path, input and output tokens come from llama-server's own tokenizer (`/tokenize` or the response `usage`), and output length is forced to `max_tokens`, so offered demand = input + 2 × max_tokens is exact. Only the simulated path counts whitespace-separated words, and it is labelled simulated. Weighted tokens = input + 2 × output (VTC weights).
3. The simulated engine of the appendix is calibrated to the real replica (`results/real/profile.md`, check ±25%); the built-in default engine (10-replica, GPU-like) is illustrative. Every result produced with either is labelled **simulated**; the readiness hold is labelled **injected**.
4. No public trace has tenant IDs; tenants are synthetic (Gamma arrivals, CV > 1). The BurstGPT / Azure CSV schemas can be loaded for token lengths if the user downloads them.
5. "CPU-HPA" is approximated by engine slot occupancy (busy fraction), because CPU % is not modelled. Labelled in results.

## Risks

| Risk | Mitigation |
|---|---|
| RAM headroom (~8 GB free) during client-go compile | `-p 4`, guard before every heavy step, one heavy step at a time |
| Kubernetes path not runnable here | unit tests with client-go fakes + manifest validation; k3s live smoke in WSL2 only after approval (N7, Q15-K); README records the exact reason if blocked |
| Simulation fidelity questioned | same scheduler/controller code in sim and live; engine calibrated to the real replica; live-vs-sim cross-check (R20); threats-to-validity section |
| Real results are laptop-scale | budget of 4 real replicas, each on one physical P-core, Qwen2.5-0.5B on CPU; claims are relative (setups at equal budget, same trace, interleaved runs), never absolute GPU-fleet numbers |
| Cold start too short to matter (~0.6 s warm) | readiness hold T = 30 s, labelled "injected load delay (emulates network model load); inference is real"; T = 0 reported too (Q12) |
| Run-to-run drift (thermal, background load) | seeded interleaved order, drift check first vs last run per setup, CPU frequency/temperature logged per run when readable (Q13, Q14) |
| Windows process cleanup | demo runs all components in one process (one PID); e2e tests kill by PID |
| Parallel build agents editing `go.mod` | all dependencies added once in the skeleton; agents must not run `go get`/`go mod tidy` |

---

## Milestones

- **M1 Skeleton:** module, binary with subcommands, config, run/build/test/lint scripts, landing page + `/healthz`; build passes.
- **M2 Core flow end-to-end:** arbiter, estimate, stabilize, fairq, engine/replica/pool, gateway, controller wired locally; minimal dashboard.
- **M3 Remaining must-haves, then should-haves:** K8s controller + CRD + manifests, DES + experiment + eval metrics, forecaster, baselines S1–S7.
- **M4 UI/UX polish:** two-panel dashboard, decision log, results page, states, keyboard, contrast.
- **M5 Hardening:** validation, error handling, logging, edge cases, demo seed scenario.
- **M6 Evaluation:** experiment runs, forecaster held-out evaluation, leakage check, failure analysis, README numbers.

After each milestone: guard → build → tests → smoke run → update this matrix → commit.

**BRIEF-v2 milestones** (`docs/BRIEF-v2.md`; same per-milestone routine):
- **N1** Reconcile the docs — **done** (decisions Q9–Q18, R25, assumptions 2–3, diagrams, risks/threats; no Kaggle tier).
- **N2** P/E-core detection + pinning + re-profile; C, TPOT p99 and cold start in the config; SLOs (TTFT p99 ≤ 2 s, TPOT p99 bound derived in DESIGN) — **done** (see M2 below). **Checkpoint: stop and show the user before any headline run.**
- **N3** Offered-demand estimator + S8 + spiral tests (test first) — **done**; finding Q11-R: no spiral in the full system.
- **N4** Real harness: S4 + readiness hold + interleaved runner + per-run CSV + tokenizer counting — **done**.
- **N5** `loadgen` live-demo subcommand + cross-compiled binaries + firewall script + DEMO.md — **done** (firewall script written, not run).
- **N6** Headline real runs, simulated appendix, forecaster evaluation, `results/real/summary.md`, README from measured results.
- **N7** Kubernetes live smoke on k3s (checkpoint: ask before installing) — **done** 2026-10-28 with simulated replicas (`results/k8s/`).
- Cut order: stretch mode → real cold-start sweep (keep simulated) → N7 → forecaster. Never cut: S4 in real runs, S8, readiness hold, dispatch window, interleaved runs.

---

## Decisions

| # | Question / decision | Answer | Consequence |
|---|---|---|---|
| Q1 | How to deliver/verify the Kubernetes part without Docker/kind? | **Local mode + K8s code (Recommended)** | Full CRD/controller/manifests unit-tested with fakes; live kind run marked Blocked in README |
| Q2 | What serves the model replicas? | **Simulated engine + real adapter (Recommended)** | Go simulated engine labelled SIMULATED; gateway can point at any OpenAI-compatible server |
| Q3 | How to produce the 5-setup × 5-run evaluation? | **Virtual-time sim + live check (Recommended)** | DES reuses real fairq/controller code; one live run cross-checks; results labelled simulated |
| Q4 | Which ML component? | **Demand forecaster (Recommended)** | Holt-Winters with reactive fallback; MAPE on held-out split |
| D1 | Git repo location (decided) | Nested repo in this folder | Parent git root is `C:/Users/nilay` (home); a nested repo keeps commits and fresh-clone tests clean. No remotes, no push. |
| D2 | Controller→gateway mechanism (decided, FW 3.4) | HTTP push of a versioned policy every cycle; gateway keeps last-known-good | measurable propagation latency; avoids ConfigMap volume delay |
| D3 | E2E smoke test style (decided) | Go test driving the same HTTP API the dashboard buttons call, headless, `-p 1` | no browser download needed (Playwright not installed) |
| D4 | Lint (decided) | `gofmt -l` empty + `go vet ./...` + `node --check` on JS + `python -m py_compile` | no golangci-lint installed; these are available offline |
| U1 | User: "we need real implementation… if we want real metrics" | Clarified: **real metrics from real inference** (supersedes Q2's simulated-only replicas and changes Q3) | All code was already real; what changes is the model compute and where the headline numbers come from |
| Q5 | Which real inference engine? | **llama.cpp on GPU (Recommended)** | Replica = real `llama-server` process with Qwen2.5-0.5B-Instruct Q4 GGUF on the RTX 3050; real streaming, batching and model-load cold start; replica budget = what fits in ≤ 70% of free VRAM (measured) |
| Q6 | What should the evaluation report? | **Real core + labelled sim (Recommended)** | Headline numbers from REAL runs: S2 KEDA, S3 static partition, S5 no-gateway ablation, S7 TenantScaler, 5 runs each; the 7-setup / cold-start sweep stays as a clearly labelled simulated appendix |
| Q7 | How should the live demo compare stacks with ~4 real replicas? | **Sequential A/B (Recommended)** | Same recorded traffic runs on the baseline, then on TenantScaler, each with the full replica budget; dashboard shows both panels side by side |
| D5 | Model and cache location (decided) | Qwen2.5-0.5B-Instruct Q4_K_M (Apache-2.0); downloads in `.cache/` (gitignored), pinned versions + SHA-256 check in `scripts/setup-llama.*` | smallest instruct model that streams real tokens fast; leaves VRAM for several replicas |
| M1 | Measured (real llama.cpp b11898, Qwen2.5-0.5B Q4_K_M, unique prompts, 4 slots/replica) | GPU: 549 MiB/replica, cold start 3.9 s first then ~0.6 s; aggregate **flat ~630 tok/s for 1→6 replicas** (TPOT 5.6→34.3 ms). CPU pinned 1 physical core/replica: **66→127→169→214 tok/s for 1→4 replicas**, TPOT 59–67 ms | on one GPU a replica adds no capacity, so GPU replicas would make every autoscaling result an artifact |
| Q8 | Flagged per §0.1: the GPU choice (Q5) is worse given M1. Where should real replicas run? | **CPU-pinned replicas (Recommended)** | Real llama.cpp + Qwen 0.5B, each replica pinned to one physical core (2 threads), budget **4 replicas** (= the N = 8 thread cap); GPU unused. Guarantees 1/1/1 of 4. Simulated engine parameters are calibrated from the real profile. |
| D6 | Fixes found on real replicas (`docs/DESIGN.md` §19) | Gateway weight = max(grant, guarantee); over-share shedding only under contention; slot reservation for shedding tenants (+ S7nr ablation); upstream keep-alive retry; separate prefill load factor | Each has a regression test that fails without it; committed in 8ed96d9 |
| U2 | User (2026-10-27): stopped the build, then proposed a Kaggle GPU tier with tunnels and three teammate APIs | Explained feasibility; started gateway API keys | Superseded by BRIEF-v2 (Q9 cuts the GPU tier); API keys kept for Q16 |
| Q9 | BRIEF-v2: GPU/Kaggle tier | **Cut.** M1 measured flat aggregate throughput on one shared GPU (~630 tok/s for 1–6 replicas); a T4 would repeat that. No tunnels anywhere | Real replicas stay CPU-pinned; the per-GPU replica option was reverted before commit |
| Q10 | BRIEF-v2: real headline set | **S2, S3, S4, S5, S7** (S4 vs S7 is the novelty test); simulated appendix S1–S7 + S8 | R27, R30 |
| Q11 | BRIEF-v2: demand signal | Demand = OFFERED weighted tokens at gateway admission, shed and queued included; output tokens = `max_tokens` (forced lengths). S8 = naive demand (admitted/served only), tests first | R31, R37. Assumption: the gateway already counts every arrival (shed included) as demand, so N3 adds S8 and the spiral tests rather than changing the default |
| Q12 | BRIEF-v2: cold start | Readiness hold T (process starts, model loads, Ready only after T); label "injected load delay (emulates network model load); inference is real"; headline at T = 30 s and T = 0; sweep 10/30/60/120 s real if time allows, else simulated | R16, R32 |
| Q13 | BRIEF-v2: CPU topology | Detect P/E cores; one replica per physical P-core; services on the remaining cores; re-profile; budget 4 unless the profile says otherwise; log CPU frequency/temperature if readable | R33 |
| Q14 | BRIEF-v2: run order | Seeded random setup order per run index (interleaved); every run + mean + 95% CI (t) + drift check | R34 |
| Q15 | BRIEF-v2: pre-registration | README states before the first headline run: "During C's spike, at equal replica budget, S7 has higher victim (A, B) SLO attainment (TTFT p99 and TPOT both met) than S4, with C's shed % and shed-aware attainment reported alongside." Reported whichever way it goes | R35 |
| Q15-K | BRIEF-v2: Kubernetes | R23 promoted to a k3s live smoke (N7), only after the user approves the install | R23 → Q15-K |
| Q16 | BRIEF-v2: live demo | `tenantscaler loadgen` for teammates' laptops (cross-compiled), identity and max priority from the API key, gateway may bind 0.0.0.0, firewall script run only with approval; the dashboard labels it live demo; headline runs use only the local pinned loadgen | R36 |
| Q17 | BRIEF-v2: token counting | llama-server tokenizer on the real path; whitespace counting only on the simulated path, labelled | R37 |
| Q18 | BRIEF-v2: forecaster evaluation | Held-out, time-ordered segment of a real rate series (Azure 2024 or BurstGPT, cited) AND synthetic B, vs seasonal-naive; MAPE; leakage check; effect on B even if null | R17; the trace download needs approval |
| Q11-R | N3 result (2026-10-27): the predicted S8 death spiral did not occur. Tests written first (c3d68df): the control-core unit test shows the mechanism (with a gateway that sheds C's whole sheddable share, naive demand drops C's grant 8 → 4 and the desired pool 10 → 6 while C's offered load is unchanged; offered demand holds 8/10); the simulator test required S8 to run fewer replicas than S7 and lose more of C, and S8 matched S7 exactly (budget 6: 6.00 replicas, 45.6% of C lost in both). SIMULATED, 5 runs: identical at budgets 10 and 8, within noise at budget 6 and on both real traces; with the pre-F2 always-on shedding S8 still did not shrink the pool (it flapped and shed less of C: 22% vs 42% at budget 10) | User chose **report as found (Recommended)** | Root cause: TenantScaler's shed decision uses the same demand estimate, so a low admitted-only reading switches shedding off and recovers (self-correcting); shedding removes only the excess. The spiral needs shedding independent of the scaling signal, as in S4 (`TestFlowControlMetricBlindSpot`). The simulator test now records this; README/CLAIMS must not claim "TenantScaler prevents a death spiral" |
| Q18-A | Approval (2026-10-27): which real trace for the forecaster? | **Azure 2024 trace (Recommended)** | `AzureLLMInferenceTrace_code_1week.csv` from github.com/Azure/AzurePublicDataset release `dataset-llm-2024` (692 MB by HEAD, under the 1 GB limit), CC-BY, cite Stojkovic et al., "DynamoLLM", HPCA 2025; stored in `.cache/traces/` (git-ignored); downloaded 2026-10-27, 691,989,454 bytes, SHA-256 `71de5c55cbc35f8f1ed0b6b7806b4cd1e9764b0058469725a6aac98023a1448f`, rows from 2024-05-10 00:00 to 2024-05-16 23:59 UTC (7 days); `scripts/get-trace.ps1` reproduces it; only TIMESTAMP and token counts used |
| N7-A | Approval (2026-10-27): WSL2 is not installed; k3s needs it (admin + reboot) | **Install after the runs** | After the headline runs the owner runs `wsl --install` as Administrator and reboots; a new session installs k3s (CPU only), 3-hour time box |
| S1 | BRIEF-v2: stretch | Degrade-before-drop as a separate mode only, never in the headline | R39 |
| M2 | Measured 2026-10-27 (N2, REAL, `results/real/scaling.md`, `profile.md`) | Topology: 6 P-cores (logical 0–11, 2 threads each) + 4 E-cores (12–15). Replicas pinned one per P-core (CPUs 0-1 / 2-3 / 4-5 / 6-7, verified from outside: llama-server affinity 0x0003), services on CPUs 8–15 (tenantscaler affinity 0xff00). Scaling curve, 4 concurrent requests per replica: 133.6 / 261.3 / 371.4 / 466.7 weighted tok/s for 1–4 replicas (efficiency 1.00 / 0.98 / 0.93 / 0.87); pooled p99 TPOT with 4 replicas 156.6 ms, p99 TTFT 1.754 s. Single replica: cold start 605 ms, C = 136 at concurrency 4. CPU temperature not readable without admin; frequency logged as % of nominal | **C = 117** (466.7 / 4), **SLO TTFT p99 ≤ 2 s, TPOT p99 ≤ 200 ms** (1.25 × 156.6 ms, rounded up; rules in DESIGN §20, fixed before the measurement), **budget stays 4**; written to `config/real.yaml` |
| R1 | Risk accepted with Q5/Q6 (updated for Q8, Q12) | Real numbers are laptop-scale (0.5B model on 4 CPU-pinned replicas) and the real cold start is under a second warm, so it is emulated with the labelled readiness hold | the gateway-coupling benefit measured on real replicas may be smaller than in simulation; reported as measured |
