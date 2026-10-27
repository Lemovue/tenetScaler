# Phase 1 status: kitna ready hai, kitna bacha hai, K8s kab, aur comparison kaise ho raha hai

_Status: 2026-10-27, 17:25 (first written 15:00). Headline runs: T = 30 s finished (25 of 25), T = 0 running (5 of 25, done about 18:50, estimate). Every number here is either measured (with its file) or labelled SIMULATED / single run. Times marked "estimate" are estimates, not measurements._

---

## 1. Kitna kaam ready hai (by phase)

Legend: ✅ done · 🟡 started or running · ⬜ not started.

### Phase 1: Core logic: 9 of 9 ✅

Arbiter (guarantee → borrow → reclaim), demand estimate, stabilizer, control cycle with plain-English decisions, fail-static, fair queue (VTC counters, priorities, deadlines, critical quota), 429 + Retry-After only while the pool is busy, config validation, Holt-Winters forecaster.

### Phase 2: Working system: 10 of 11

| Item | Status |
|---|---|
| Real llama.cpp replicas, killed when TenantScaler exits | ✅ |
| Gateway, controller, pool services | ✅ |
| Dashboard: same traffic on KEDA, then TenantScaler | ✅ |
| API keys: key = tenant + highest priority | ✅ |
| Kubernetes code (CRD, controller, RBAC, manifests), tested with fakes | ✅ |
| Fixes found on real replicas (starved idle team, over-shedding, 502s, slot reservation) | ✅ |
| CPU detection: 6 P-cores + 4 E-cores | ✅ |
| One replica per P-core, services on the other cores (verified live) | ✅ |
| Live demo: teammates' laptops send traffic with `tenantscaler loadgen` (Windows/Mac/Linux builds in `dist/`) | ✅ |
| Live Kubernetes cluster (k3s) | ✅ smoke test, simulated replicas (section 3) |

### Phase 3: Proof (measurements): 9 of 11 done, 2 running

| Item | Status | Where |
|---|---|---|
| Real profile of one pinned replica | ✅ | `results/real/profile.md` |
| 1–4 replica throughput curve + SLO derivation | ✅ | `results/real/scaling.md` |
| Simulator calibrated to the real replica | ✅ | `config/real.yaml` |
| S8 death-spiral ablation, tests first | ✅ (finding: no spiral in our system, see section 4) | `PLAN.md` Q11-R |
| Readiness hold T (injected 30 s load delay, labelled) | ✅ | `docs/DESIGN.md` §22 |
| S4 (Flow-Control-style baseline) on real replicas | ✅ | harness |
| Seeded interleaved run order + drift check | ✅ | harness |
| Real tokenizer counts (0 fallbacks in the smoke run) | ✅ | harness |
| Main claim pre-registered in the README before the runs | ✅ | `README.md` |
| Headline real runs: 5 setups × 5 runs × (T = 30 s and T = 0) | 🟡 T = 30 s ✅ 25/25 (claim **supported**, section 4); T = 0 running, 5/25 | `results/real/hold30`, `results/real/hold0` |
| Simulated appendix (S1–S8 + cold-start sweep) and forecaster on the real Azure trace | 🟡 code written; runs after the headline runs; trace downloaded and verified (`scripts/get-trace.ps1`) | `results/sim/`, `results/forecast-real/` |

### Phase 4: Demo and pitch: 4 of 10

| Item | Status |
|---|---|
| Dashboard A/B demo on real replicas | ✅ |
| Dashboard spider-web theme (dark default, light, spider theme toggle, Calm switch, MET / MISSED / SHED labels) | 🟡 built on branch `ui-spider-theme`; browser checks pass; `go vet` + tests after the runs; **waiting for your review before merge** (section 5) |
| Three-team API test on real replicas | ✅ |
| `DEMO.md` (3-minute script, phone hotspot, backup video) | ✅ |
| Firewall script for the live demo | ✅ written (run only with your OK) |
| README final numbers + `CLAIMS.md` complete | 🟡 (`CLAIMS.md` started) |
| End-to-end tests (`test/e2e`) | ⬜ |
| Backup video | ⬜ (team) |
| Slides with measured numbers | ⬜ (team, after the runs) |
| Two full mock presentations | ⬜ (team) |

**Overall: 32 of 41 items done (78%), 4 in progress.** One new item (the theme). Code and tooling are essentially complete. What is left is mainly the second half of the running experiment, the write-up of its results, Kubernetes live, the theme review, and the team's demo prep.

---

## 2. Kya bacha hai, kis order mein

| # | Task | Who | When (estimate) |
|---|---|---|---|
| 1 | Finish the headline real runs (T = 30 s, then T = 0; 50 runs) | automatic | T = 30 s ✅ done 16:54; T = 0 5/25 at 17:20, about 4.5 min per run, so about 18:50 (estimate) |
| 2 | Trace download (692 MB), SHA-256 recorded in `PLAN.md` Q18-A | automatic | ✅ done 15:30 |
| 2b | Spider theme: `go vet`, tests and a check with data, then commit on `ui-spider-theme`; merge only after your review | Harshit, then team review | right after the runs |
| 3 | Compile and test the new forecaster code, run the real-trace evaluation and the simulated appendix | Mayank, Akshat | right after the runs; minutes |
| 4 | README results + S7 vs S4 verdict (reported whichever way it goes); `CLAIMS.md` complete; PLAN matrix final | Nilay | after 3 |
| 5 | End-to-end tests | Harshit | ✅ done (`test/e2e`) |
| 6 | Kubernetes live smoke (N7) | Nilay + Akshat | section 3 |
| 7 | Backup video, slides, two mocks | team | after the numbers exist |

Code is not compiled while the real runs are going: compiling loads the CPU cores the replicas are measured on.

---

## 3. K8s actual environment kab run hoga

**Done on 2026-10-28** (update): WSL2 + Ubuntu 24.04 + k3s v1.36.5 on this laptop. Runbook: `deploy/k3s/README.md`. Evidence: `results/k8s/`.

What differs from the plan below:
- The cluster runs the **simulated** replicas (`deploy/k8s/10-model-pool.yaml`), not llama-server.
- The image is built without Docker (`scripts/k3s/image.sh`), so no llama.cpp image and no Docker install were needed.
- The controller scaled the pool from 1 to 8 and back, not from 1 to 3.

The original plan, kept for the record:

**Not yet: it needs WSL2, which is not installed on this laptop.** You chose (decision N7-A in `PLAN.md`) to install it **after the headline runs**, because installing it needs a reboot, and a reboot would kill the running experiment.

Steps, in order:

1. **After the runs finish:** open PowerShell **as Administrator** and run `wsl --install`, then **reboot**.
2. **After the reboot:** Ubuntu in WSL2, then k3s (a light Kubernetes, CPU only, no NVIDIA).
3. **What the demo will show** (brief N7):
   - The TenantScaler controller scales a llama-server Deployment from 1 to 3 replicas when C spikes.
   - The `TenantScaler` custom resource's status updates with the decisions.
   - The gateway finds the new replicas through EndpointSlices.
4. **Time box: 3 hours.** If it doesn't work in that time, the README records exactly why.
5. **Approvals needed then**, because each is an install or a download:
   - the Ubuntu image (part of `wsl --install`)
   - k3s itself
   - a llama.cpp server container image
   - building our own container image from the existing `Dockerfile`

What exists already: the CRD, RBAC, Deployments, KEDA/HPA baseline manifests and the Kubernetes controller code, all tested with Kubernetes fakes (`internal/k8s`, `deploy/k8s`). The headline numbers do **not** depend on Kubernetes: they come from the same controller and gateway code running against local real replicas.

---

## 4. Approach aur benchmarks kaise compare ho rahe hain

### What we compare

| Setup | What it is | Where it runs |
|---|---|---|
| S1 | CPU-style HPA | simulated |
| **S2** | KEDA on queue length: today's common answer | **real** |
| **S3** | Static partition: each team owns fixed replicas | **real** |
| **S4** | **Flow-Control-style**: per-tenant round-robin gateway, priority bands, 429 for sheddable overflow, KEDA on the gateway queue (closest open-source design, llm-d) | **real** |
| **S5** | TenantScaler without the gateway (arbiter only) | **real** |
| S6 | TenantScaler without borrowing | simulated |
| **S7** | **TenantScaler, full** | **real** |
| S7F | S7 + Holt-Winters forecaster | simulated |
| S8 | S7 with naive demand (death-spiral ablation) | simulated |

**S7 vs S4 is the main test.** It asks whether TenantScaler's guaranteed shares, borrowing and slot reservation beat a design that already has per-tenant queues and 429. Against S2 alone the result would be easy and not convincing.

### How we keep it fair

- **Same work for everyone:** the same recorded trace (seed 1 + k for run k), the same model, the same budget of 4 replicas (one per P-core), output lengths forced, input tokens counted by the replica's own tokenizer.
- **Same machine conditions:** setups run in a **seeded random order** inside each run index, so heat or background load doesn't always hit the same setup. CPU frequency is logged per run, and a **drift check** compares each setup's first and last run.
- **Repetition:** at least 5 runs per setup, each run reported, mean ± 95% confidence interval (Student t).
- **Cold start:** real replicas load in about 0.6 s, which hides the cold-start problem. So the main runs add a **30 s injected load delay** (labelled everywhere: "injected load delay (emulates network model load); inference is real"), and the same runs are repeated **without it (T = 0)**.
- **Pre-registered:** the claim and the decision rule were written in the README and committed **before** the first headline run (commit 5e40366).

### The pre-registered decision rule

- **Metric:** victim (A, B) SLO attainment during C's spike, i.e. the share of their requests served with TTFT ≤ 2 s **and** TPOT ≤ 200 ms.
- **Comparison:** S7 − S4, paired by run index (same trace).
- **Verdict:** **supported** if the 95% CI of the mean difference is wholly above 0; **contradicted** if wholly below; otherwise **not shown**.
- **Reported alongside:** C's shed %, the victims' shed-aware attainment, and replica-minutes, so that "victims did better because C was shed a lot" is visible.

### Other metrics (every setup)

Victim SLO violation %, shed-aware violation %, noisy tenant's shed %, reaction time, replica-minutes, Jain fairness index, per-tenant p99 TTFT and TPOT, controller-to-gateway policy latency.

### Early numbers we already have (not the final result)

| What | Number | Type |
|---|---|---|
| 1 / 2 / 3 / 4 pinned replicas | 133.6 / 261.3 / 371.4 / 466.7 weighted tok/s | REAL, `results/real/scaling.csv` |
| SLO derived from that curve | TTFT p99 ≤ 2 s, TPOT p99 ≤ 200 ms; C = 117 per replica | REAL (derived), `results/real/scaling.md` |
| Smoke run, all 5 setups, T = 30 s, victims missing the SLO | S2 95.7%, S3 21.7%, S4 65.2%, S5 82.6%, S7 26.1% | REAL, **one run only**, `.run/n4-smoke/summary.md` |
| Live three-laptop test | A and B: 0 × 429; C: 67 × 429 | REAL, one run |
| Simulated, tight budget 6 | a Flow-Control-style variant (S4b) slightly better than S7 for victims (0.8% vs 3.1%) | SIMULATED, 10 runs |
| S8 death spiral | did **not** happen in our system (S8 ≈ S7); the shed decision uses the same demand number, so it corrects itself | SIMULATED, `PLAN.md` Q11-R |

### First headline result: T = 30 s (REAL inference, INJECTED 30 s load delay)

From `results/real/hold30/summary.md` (5 runs per setup, seeded interleaved order):

| Run | S7 victim attainment | S4 victim attainment | S7 − S4 |
|---|---|---|---|
| 0 | 77.8% | 53.3% | +24.4 pp |
| 1 | 88.6% | 78.1% | +10.4 pp |
| 2 | 53.4% | 41.4% | +12.1 pp |
| 3 | 71.1% | 38.6% | +32.5 pp |
| 4 | 72.4% | 44.6% | +27.8 pp |

**Mean S7 − S4 = +21.4 percentage points, 95% CI [9.3, 33.5], N = 5 pairs. Pre-registered verdict: supported.** Report it with its context: S7 shed 36–54% of the noisy tenant C's requests (S4: 51–60%), and S7 used more replica-minutes (14.3–15.8 vs 10.2–12.4), so part of the gain comes from using more of the 4-replica budget.

The T = 0 verdict (no injected delay) comes from `results/real/hold0/summary.md` when the second half finishes; it is reported whichever way it goes. **Don't quote the smoke-run numbers as results.**

---

## 5. Naya frontend (spider-web theme) kaise chalayein

It lives in a separate git worktree, so the main tree (backend work) is untouched:
`C:\Users\nilay\OneDrive\Desktop\TenantScaler-ui-spider-theme` (branch `ui-spider-theme`). Only `internal/web/` and one README line changed.

**Quick look, no build, no CPU cost** (works now): open this file in Edge or Chrome:
`C:\Users\nilay\OneDrive\Desktop\TenantScaler-ui-spider-theme\internal\web\static\index.html`.
You see both themes, the webs, the spider toggle and Calm. There is no data, so a "Cannot reach the demo server" banner shows; that is expected.

**Full dashboard with data** (after the T = 0 runs finish, about 18:50; building now would load the CPU the experiment is measuring):

```powershell
cd "C:\Users\nilay\OneDrive\Desktop\TenantScaler-ui-spider-theme"
go build -o bin/tenantscaler.exe ./cmd/tenantscaler
bin/tenantscaler.exe demo -backend simulated -addr 127.0.0.1:8090
```

Open http://127.0.0.1:8090 and press **run a/b** (about 4–5 min for both stacks; add `-scenario tiny` for a 6 s run). `-backend simulated` keeps it off the real replicas, and the page says SIMULATED. Port 8090 keeps it apart from the main tree's demo on 8080. Ctrl+C stops it.

**What to check:**

1. The spider at the top right: blue in dark, red in light. Click it, or Tab to it and press Enter or Space. It drops, the theme crossfades, and it climbs back. Reload: your choice stays.
2. Calm (top bar): the webs stop moving. During a run, Calm switches itself on ("on during the A/B run").
3. Per-tenant tables: the SLO status column shows MET / MISSED / SHED with icons.
4. Results tab: SIMULATED / REAL / INJECTED badges.
5. CPU: open Task Manager → Details, find `msedge.exe` (or `chrome.exe`) and compare the CPU column with Calm on and off. Expected: close to 0% either way; the sway updates at most 30 times a second.
6. Automated check (headless Edge, on CPUs 12–15): `./internal/web/check-theme.ps1`, or `-Url http://127.0.0.1:8090/` against the running demo. All 16 checks should print PASS.

Contrast ratios and design notes: `internal/web/THEME_NOTES.md`.
