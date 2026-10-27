# Claims and their evidence

Every claim made in the pitch, the README or the team docs, mapped to the file that supports it. A claim not listed here must not be made. **REAL** = measured on real llama.cpp replicas; **SIMULATED** = discrete-event simulator (labelled wherever shown); **INJECTED** = the readiness hold, a load delay we add on purpose (inference stays real).

| # | Claim | Type | Evidence (file, rows) |
|---|---|---|---|
| C1 | The laptop has 6 P-cores (logical 0–11, 2 threads each) and 4 E-cores (12–15); each replica runs on its own P-core, the services on CPUs 8–15. | REAL | `docs/DESIGN.md` §20; `internal/topology` test `TestDetectThisMachine`; `results/real/profile.md` label |
| C2 | 1 / 2 / 3 / 4 pinned replicas serve 133.6 / 261.3 / 371.4 / 466.7 weighted tokens/s (efficiency 1.00 / 0.98 / 0.93 / 0.87). | REAL | `results/real/scaling.csv` (rows `replica = all`), `results/real/scaling.md` |
| C3 | The SLO is TTFT p99 ≤ 2 s and TPOT p99 ≤ 200 ms, derived from the measured curve (1.25 × 156.6 ms, rounded up) by a rule fixed before the measurement. | REAL (derived) | `results/real/scaling.md` (derivation), `docs/DESIGN.md` §20 |
| C4 | One replica serves C = 117 weighted tokens/s while the whole budget is busy (136 alone) and starts in about 0.6 s. | REAL | `results/real/scaling.csv` (4 replicas, `all`), `results/real/profile.md` |
| C5 | On one shared laptop GPU, 1–6 replicas gave flat total throughput (~630 tok/s), so extra GPU replicas add no capacity there. | REAL (M1 probe) | `PLAN.md` decision M1 (the probe's raw output was not kept as a CSV; quote it as the M1 measurement) |
| C6 | Our naive-demand ablation (S8) does **not** death-spiral in the full system; the shed decision uses the same demand estimate and corrects itself. The spiral shape exists in Flow-Control-style designs. | SIMULATED | `PLAN.md` Q11-R; `internal/sim` tests `TestNaiveDemandDoesNotSpiralInSimulation`, `TestFlowControlMetricBlindSpot`; `internal/control` `TestNaiveDemandSpirals` |
| C7 | At the default simulated budget (10 replicas), victims miss their SLO 34.4% of the time under KEDA and 0% under TenantScaler. | SIMULATED | `results/sim/default/runs.csv`, `results/sim/default/summary.md` |
| C8 | At a tight simulated budget (6), a Flow-Control-style baseline with KEDA on outstanding requests (S4b) is slightly better for victims than TenantScaler (0.8% vs 3.1%). | SIMULATED | `results/sim/budget6/runs.csv`, `results/sim/budget6/summary.md` |
| C9 | The Holt-Winters forecaster is not significantly more accurate than seasonal-naive on the periodic tenant (MAPE 47.7% vs 49.6%). | SIMULATED | `results/sim/default/forecast.md` |
| C10 | Headline S7 vs S4 claim (pre-registered in README) | REAL | pending: `results/real/hold30/summary.md` and `results/real/hold0/summary.md` (N6) |
