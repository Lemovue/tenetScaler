# Akshat: Platform, Experiments and Competition

_Overall status: `00_PROJECT_STATUS.md`. The old script is `TenantScaler_Final/06_Akshat.pdf`; where the two differ, this file is newer._

## Tumhara role

You show **that the results are real and the comparison is fair**, and you know the competition (llm-d, KEDA, Kueue, TRE) better than anyone in the room.

## Platform: kya chal raha hai

- **Model:** Qwen2.5-0.5B-Instruct (Q4_K_M), served by llama.cpp `llama-server` build b11898. Both are downloaded with pinned SHA-256 hashes by `scripts/setup-llama.ps1`.
- **Budget:** 4 replicas. Each has 2 threads and 4 request slots.
- **CPU:** detected as **6 P-cores** (logical CPUs 0–11) and **4 E-cores** (12–15). Each replica gets its own P-core; the gateway, controller and load generator get the rest. Pinning is in progress (N2).
- **Measured:** 1 / 2 / 3 / 4 pinned replicas serve **133.6 / 261.3 / 371.4 / 466.7** weighted tokens/s (`results/real/scaling.md`); a replica starts in **~0.6 s**. SLO derived from this: TTFT p99 ≤ 2 s, TPOT p99 ≤ 200 ms.
- **Why not GPU:** on the laptop GPU, 1–6 replicas gave the same total throughput (~630 tok/s, measured). The Kaggle GPU plan is cut (decision Q9).
- **Cold start:** real start-up is too fast to matter, so real runs add a **readiness hold** of T = 30 s. Always say: "injected load delay that emulates network model load; inference is real". We report T = 0 too.

## Code kahan hai

| What | Where |
|---|---|
| Real replicas, kill-on-exit, CPU pinning | `internal/llama`, `internal/topology` |
| Pool manager | `internal/pool` |
| One live stack per setup | `internal/stack` |
| Real experiment runner + real profile | `internal/realrun` (`tenantscaler realrun`, `tenantscaler profile`) |
| Simulator + simulated experiment | `internal/sim`, `internal/experiment` (`tenantscaler experiment`) |
| Metrics, 95% CI, charts | `internal/eval` |
| Kubernetes: CRD, RBAC, Deployments, KEDA/HPA baselines | `deploy/k8s/`, `internal/k8s` |

## The setups (yaad karo)

| Setup | What it is | Real? |
|---|---|---|
| S1 | CPU-style HPA | simulated only |
| S2 | KEDA on queue length (today's common answer) | **real** |
| S3 | Static partition: each team owns its replicas | **real** |
| S4 | **llm-d Flow-Control-like**: round-robin per tenant, priority bands, 429, KEDA on the gateway queue | **real**. **S7 vs S4 is the novelty test** |
| S5 | TenantScaler without the gateway (arbiter only) | **real** |
| S6 | TenantScaler without borrowing | simulated |
| S7 | **TenantScaler, full** | **real** |
| S8 | TenantScaler with naive demand (death-spiral ablation) | simulated |

**Fairness:**
- same recorded traffic and seed for every setup
- same model and budget
- setups run in a **seeded random interleaved order**, at least 5 runs each
- every run is reported, with mean ± 95% CI and a first-vs-last **drift check**
- CPU frequency will be logged per run (N4)
- CPU temperature is **not readable** without admin; we say so

## Competition (from `framework.txt`)

| Tool | What it already does | Our difference |
|---|---|---|
| **llm-d Flow Control** (EPP) | Per-tenant queues by fairness ID, priority bands, 429 for sheddable traffic, queues to hide cold start | No guaranteed shares that move with the autoscaler; round-robin gives each tenant an equal turn. That is our S4 baseline |
| **KEDA / HPA** | Scale on queue or CPU | Decide *how many*, not *who* (S2, S1) |
| **Kueue** | Quota + borrowing + preemption for serving Deployments | Per pod and per model, not per tenant inside one model; we reclaim at request level instead of preempting pods |
| **TRE** (arXiv 2609.29160) | Moves replicas between *models* under a GPU budget | Its abstract mentions no tenants inside one model and no admission or shedding (we read the abstract only; say so) |
| **WVA** | – | Deprecated in llm-d v1.0 |

## Sawaal: English jawab

_These answers describe the finished system. Anything still marked ⬜ in `00_PROJECT_STATUS.md` must be done before you say it on stage._

| Sawaal | Jawab |
|---|---|
| "How did you make the comparison fair?" | "Same trace and seed, same model and budget, setups in a seeded interleaved order, at least five runs each, every run reported with t-based 95% intervals and a drift check." |
| "Isn't the cold start fake?" | "The 30-second hold is injected and labelled as such; inference is real. We also report the runs without it." |
| "Why only 4 replicas?" | "One replica per physical performance core of the laptop. The claim is relative: setups at equal budget. Absolute numbers will differ on a GPU fleet." |
| "Did you run Kubernetes?" | "Yes, a smoke test on a real cluster: k3s in WSL2 on the laptop, one node. The controller scaled the pool Deployment up and down, wrote each tenant's decision into its TenantScaler status, and the gateway followed the pods through EndpointSlices. With the laptop's sizing the noisy tenant was shed while a and b were never rejected. The replicas in the cluster are simulated, not llama.cpp, so those latencies are not results. Evidence: `results/k8s/`." |

## Tumhare liye kya baaki hai (phase-wise)

| Phase | Task | Milestone |
|---|---|---|
| 2–3 | **Now:** pin replicas to P-cores, re-profile, measure the 1–4 replica throughput curve, derive the TPOT SLO | N2 |
| 3 | Real harness: S4 on real replicas, readiness hold T, interleaved runner, per-run CSV | N4 |
| 3 | Headline real runs: 5 setups × ≥5 runs × (T = 30 s and T = 0). This takes several hours of an otherwise idle laptop | N6 |
| 2 | ✅ k3s live Kubernetes smoke in WSL2 (2026-10-28; `deploy/k3s/README.md`, `results/k8s/`). The PDF says Docker Desktop; what ran is k3s. | N7 |
| 4 | Run the demo end to end 3 times and record the backup video; fill the [N]/[X]/[Y]/[Z] placeholders from `results/real/summary.md` only | end |
