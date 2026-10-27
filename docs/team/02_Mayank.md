# Mayank: Controller and Arbiter (the brain)

_Overall status: `00_PROJECT_STATUS.md`. The old script is `TenantScaler_Final/04_Mayank.pdf`; where the two differ, this file is newer._

## Tumhara role

You explain the core of the solution: how the controller decides **who gets how many replicas** every 10 seconds, and why that decision is safe and explainable.

## Code kahan hai (judges ko dikhane ke liye)

| What | File |
|---|---|
| Arbiter (guarantee → borrow → shed) | `internal/arbiter/arbiter.go` (`Arbitrate`); its plain-English reasons are in `explain.go` |
| One control cycle (estimate → arbitrate → stabilize → policy → records) | `internal/control/core.go` (`Step`) |
| Demand → replicas formula | `internal/estimate/estimate.go` (`Replicas`, `Slack`) |
| Scale up fast, down slow | `internal/stabilize` |
| Live loop (reads stats, pushes policy, scales the pool) | `internal/controller/runner.go` |
| Forecaster (Holt-Winters) | `internal/forecast` |
| Real settings | `config/real.yaml` |

Run your tests: `go test ./internal/arbiter/ ./internal/control/ ./internal/estimate/ ./internal/stabilize/ ./internal/forecast/` (all pass).

## Ek cycle kaise chalti hai (real config ke numbers)

1. **Demand:** each team's weighted token rate, where weighted tokens = input + 2 × output and output = the request's `max_tokens`. We count **offered** demand: requests we shed with 429 still count. Agar shed requests ko chhod denge to demand kam dikhegi, pool shrink hoga, aur aur zyada shed hoga: ek **death spiral**.
2. **Replicas needed:** `ceil(rate × (1 + 0.2) / C)`, with C = **117** weighted tokens/s per replica (measured with all 4 real replicas busy; one alone does 136). The 0.2 is headroom.
3. **Arbiter**, with budget 4 and guarantees A = 1, B = 1, C = 1:
   1. Each team first gets `min(demand, guarantee)`.
   2. Spare replicas are lent one at a time: higher priority first, then the team closest to missing its SLO.
   3. A team that wanted more than it got has its **sheddable** requests answered with 429.
4. **Stabilizer:** scale up immediately, at most 2 replicas per cycle. Scale down only after demand stayed low for **60 s**. (The PDF says 300 s; that is KEDA's default. Our real config uses 60 s.)
5. **Policy to the gateway**, versioned: each team's weight, shed flag and slot share. Then **one plain-English record per team.**
6. **Fail-static:** if metrics are missing, nothing changes (same replicas, same policy).

**Worked example** (whiteboard pe karo): budget 10, guarantees 3/3/3, demand 2/4/8, B closest to its SLO → grants **2/4/4**, C sheds. Then A comes back with demand 3 → **3/4/3**. That is reclaim.

## Ek real bug jo humne pakda (good story for Q&A)

**Problem:** on real replicas, teams A and B waited 7–19 s while C held all the slots.

**Root cause:** in a cycle where A sent nothing, its gateway weight dropped to 0.05. So its next requests were charged 20× and it got starved when it came back.

**Fix:** a team's weight is never below its guarantee (`gatewayWeight` in `core.go`). A regression test, `TestIdleTenantKeepsGuaranteedWeight`, fails without the fix. After this and the other fixes, one real run went from 69.6% to 34.8% victim violation (n = 1).

## Forecaster: honest result

Holt-Winters error on the periodic team B is 47.7%, against 49.6% for the seasonal-naive baseline. That difference is **not significant**, and the forecaster gave no measurable SLO gain (SIMULATED, `results/sim/default/forecast.md`). Say it straight: "The reactive controller already does most of the work; the forecaster is optional and is off in the real config."

## Sawaal: English jawab

_These answers describe the finished system. Anything still marked ⬜ in `00_PROJECT_STATUS.md` must be done before you say it on stage._

| Sawaal | Jawab |
|---|---|
| "How do you estimate demand?" | "Offered weighted tokens per second, including the requests we shed, divided by what one replica serves within the SLO, which we measured: 117 weighted tokens per second with the whole budget busy." |
| "You don't know output length in advance." | "Each request states max_tokens, and we count that. In our experiments the length is forced, so the count is exact." |
| "What if guarantees add up to more than the budget?" | "We reject that configuration at load time. You can't promise more than you have." |
| "How do you stop flapping?" | "Scale up fast, scale down only after demand stays low for 60 seconds, and at most 2 replicas per step." |
| "What if the metrics source dies?" | "Fail-static: the controller holds the current pool size and policy until metrics return." |
| "Why a CRD?" | "Each team's guarantee, maximum and SLO live in a TenantScaler resource, and the controller writes its status back. A restart reads them again, so nothing changes suddenly." |

## Tumhare liye kya baaki hai (phase-wise)

| Phase | Task | Milestone |
|---|---|---|
| 3 | **Death-spiral ablation S8, done (N3).** Learn the finding: with a gateway that sheds too much, naive demand does spiral (unit test: C's grant 8 → 4, pool 10 → 6). But in our full system S8 behaves like S7, because the shed decision uses the same demand number, so a low reading switches shedding off again. The real spiral lives in S4-style designs (shedding independent of scaling). **Do not say "we prevent a death spiral"**; say "we measured it, and our loop is self-correcting by design". | N3 |
| 3 | Forecaster on a real trace segment (Azure 2024 or BurstGPT) vs seasonal-naive, plus its effect on B. The download needs Nilay's OK. | N6 |
| 4 | Code walkthrough practice: open `Arbitrate` and `Step` and explain them line by line | before the mocks |
