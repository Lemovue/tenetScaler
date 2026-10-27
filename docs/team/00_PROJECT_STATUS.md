# TenantScaler: what we are building and how far we are

_Status as of 2026-10-27, after milestone N2. The full plan is in `PLAN.md`; the current brief is `docs/BRIEF-v2.md`._

## What we are actually building (one paragraph)

Several teams share one LLM served by a **fixed budget of replicas**. When one team (C) suddenly sends a flood of requests, the other teams (A and B) get slow answers although their own traffic did not change: the **noisy neighbour**. Normal autoscalers (Kubernetes HPA, KEDA) only decide **how many** replicas to run. **TenantScaler decides who gets them.**

- **Slow loop: the controller, every 10 s.**
  1. Measures each team's demand in tokens per second.
  2. Gives each team its **guaranteed share** of the replicas, lends idle shares to busy teams and takes them back when the owner returns.
  3. Scales the pool.
  4. Explains every decision in plain English.
- **Fast loop: the gateway, on every request.**
  - Serves the teams fairly by tokens (a weighted fair queue).
  - Keeps slots free for teams inside their share.
  - Answers a team that is over its share with **HTTP 429 + Retry-After** for its low-priority ("sheddable") requests.

**What is real:** every replica is a real `llama-server` process (llama.cpp) running the Qwen2.5-0.5B model. Each replica is pinned to one CPU core of this laptop, with a budget of 4 replicas. The simulator only extends the results to setups the laptop can't run, and every simulated number is labelled **SIMULATED**.

```
tenants A, B, C ──HTTP──▶ gateway (fair queue, 429) ──▶ 4 real llama-server replicas
                              ▲ policy        │ stats
                              └── controller ◀┘  (arbiter + stabilizer) ──▶ scales the pool
                                         └──▶ decision log ──▶ dashboard
```

Code: one Go binary, `tenantscaler`, with subcommands (`demo`, `gateway`, `controller`, `pool`, `profile`, `realrun`, `experiment`, …). **29 Go packages with tests, all passing** (last full run on 2026-10-27).

## The 4 phases and how much is done

Legend: ✅ done · 🟡 started or partly done · ⬜ not started.

### Phase 1: Core logic (the brain): 9 of 9 done ✅

| Item | Status | Where |
|---|---|---|
| Arbiter: guarantees, borrowing, reclaim (worked example 2/4/4 → 3/4/3) | ✅ | `internal/arbiter` |
| Demand estimate from token rate (+ SLO slack) | ✅ | `internal/estimate` |
| Stabilizer: scale up fast, down slowly, bounded steps | ✅ | `internal/stabilize` |
| Control cycle + plain-English decision records | ✅ | `internal/control` |
| Fail-static: missing metrics ⇒ change nothing | ✅ | `internal/control` |
| Fair queue: token counters (VTC), priorities, deadlines, critical quota | ✅ | `internal/fairq` |
| 429 + jittered Retry-After, only while the pool is busy | ✅ | `internal/fairq` |
| Reject configs that promise more replicas than exist | ✅ | `internal/config` |
| Demand forecaster (Holt-Winters) with a trust gate | ✅ | `internal/forecast` |

### Phase 2: Working system (the body): 9 of 11 done

| Item | Status | Where |
|---|---|---|
| Real llama.cpp replicas, killed when TenantScaler exits | ✅ | `internal/llama`, `internal/pool` |
| Gateway HTTP service (`/v1/completions`, `/policy`, `/stats`, `/metrics`) | ✅ | `internal/gateway` |
| Controller service (reads stats, pushes policy, scales) | ✅ | `internal/controller` |
| Dashboard: same traffic on KEDA baseline, then on TenantScaler, side by side | ✅ (review fixes still open) | `internal/demo`, `internal/web` |
| API keys: one key per team, plus an admin key | ✅ | `internal/gateway/auth.go` |
| Kubernetes code: CRD, controller, RBAC, manifests (tested with fakes) | ✅ | `internal/k8s`, `deploy/k8s` |
| Fixes found on real replicas (starved idle team, over-shedding, 502s, slot reservation) | ✅ | `docs/DESIGN.md` §19 |
| Detect P-cores vs E-cores (6 P + 4 E found on this laptop) | ✅ | `internal/topology` |
| Pin each replica to one P-core, services on the other cores (verified: llama-server on CPUs 0–1, services on 8–15) | ✅ (N2) | `internal/llama`, `internal/realrun` |
| `loadgen` for teammates' laptops (live demo), built for Windows/Mac/Linux | ⬜ (N5) | |
| Live Kubernetes run (k3s v1.36.5 in WSL2, simulated replicas): pool scaled 1 → 8 → 1, status and EndpointSlices work, c shed with the demo sizing | ✅ (N7, smoke test) | `deploy/k3s`, `scripts/k3s`, `results/k8s` |

### Phase 3: Proof (measurements): 4 of 11 done

| Item | Status | Where |
|---|---|---|
| Real profile of one replica, redone with P-core pinning (C = 136 alone) | ✅ (N2) | `results/real/profile.md` |
| Simulator calibrated to the real replica (within ±25% at every load level) | ✅ | `config/real.yaml` |
| Simulated appendix (10 setups, cold-start sweep, reclaim, budget 6) | ✅ (to regenerate with S8) | `results/sim/` |
| 1–4 replica throughput curve + TPOT SLO derivation (C = 117, TPOT p99 ≤ 200 ms) | ✅ (N2) | `results/real/scaling.md` |
| S8 "death spiral" ablation, tests written first | ⬜ (N3) | |
| Readiness hold T (injected 30 s load delay, labelled) | ⬜ (N4) | |
| S4 (Flow-Control-equivalent baseline) on real replicas | ⬜ (N4) | |
| Interleaved, seeded run order + drift check | ⬜ (N4) | |
| Real tokenizer counts instead of word counts | ⬜ (N4) | |
| Main claim pre-registered in README before the runs | ⬜ (before N6) | |
| Headline real runs: S2, S3, S4, S5, S7 × ≥5 runs × (T = 30 s and T = 0), plus the forecaster on a real trace | ⬜ (N6; the trace download needs your OK) | |

### Phase 4: Demo and pitch: 2 of 9 done

| Item | Status |
|---|---|
| Dashboard A/B demo runs end to end on real replicas | ✅ |
| Three-team API test through the real gateway (curl / script) | ✅ |
| DEMO.md 3-minute script (phone hotspot, laptop plugged in, backup video) | ⬜ (N5) |
| Firewall script so teammates' laptops can connect (run only with your OK) | ⬜ (N5) |
| README with final, measured numbers + CLAIMS.md (every claim → its CSV) | ⬜ (N6) |
| End-to-end tests (`test/e2e`) | ⬜ |
| Backup video of a good run | ⬜ |
| Slides updated with measured numbers | ⬜ |
| Two full mock presentations | ⬜ |

**Overall: 24 of 40 items done (60%).** Phases 1–2 are essentially built. Most of what is left is **proof** (Phase 3) and **demo polish** (Phase 4).

## Numbers we have so far (early; final numbers come from N6)

| What | Number | Type |
|---|---|---|
| Capacity per replica with all 4 busy (one alone) | 117 (136) weighted tokens/s (input + 2 × output) | REAL, measured |
| Throughput for 1 / 2 / 3 / 4 pinned replicas | 133.6 / 261.3 / 371.4 / 466.7 weighted tokens/s | REAL, measured |
| SLO | TTFT p99 ≤ 2 s, TPOT p99 ≤ 200 ms | derived from the measured curve |
| Replica start-up time | ~0.6 s | REAL, measured |
| Dashboard A/B, victims missing the SLO during C's spike | KEDA 91.3% → TenantScaler 4.3% | REAL, **one run only (n = 1), not a result yet** |
| Victim A's p99 time-to-first-token in that run | 44.0 s → 1.8 s | REAL, n = 1 |
| Three-team test: C floods, A and B send normally | A and B 30/30 answered with no 429; C mostly 429 | REAL, n = 1 |
| Default budget (10 replicas), victims missing the SLO | KEDA 34.4% → TenantScaler 0% | SIMULATED, 10 runs |
| Tight budget (6 replicas) | TenantScaler 3.1%, S4b 0.8%: **we are not better here** | SIMULATED, 10 runs |
| Forecaster error (MAPE) on periodic team B | Holt-Winters 47.7% vs seasonal-naive 49.6%: no significant gain | SIMULATED |

Rule for everyone: **never say a number on stage that is not in this table or in `results/`, and always say whether it is real or simulated.**

## What happens next (BRIEF-v2 milestones)

| Milestone | Phase | What |
|---|---|---|
| N1 ✅ | – | Docs updated with the new decisions (committed `e02c3b4`) |
| N2 ✅ | 2–3 | Pin replicas to P-cores, 1–4 replica throughput curve, SLO derivation → **checkpoint: Nilay reviews before the headline runs** |
| N3 | 3 | Death-spiral ablation S8 (tests first) |
| N4 | 3 | Real test harness: S4, readiness hold, interleaved runs, tokenizer |
| N5 | 4 | `loadgen` for laptops, firewall script, DEMO.md |
| N6 | 3–4 | Headline real runs + final numbers in README |
| N7 ✅ | 2 | k3s live Kubernetes smoke (done 2026-10-28, simulated replicas: `results/k8s/`) |

Per-person details: `01_Nilay.md`, `02_Mayank.md`, `03_Harshit.md`, `04_Akshat.md`.
