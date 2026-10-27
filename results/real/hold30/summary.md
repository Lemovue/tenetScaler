# Evaluation on real replicas

**REAL inference — llama.cpp b11898-8b28c2394, model qwen2.5-0.5b-instruct-q4_k_m.gguf, budget of 4 replicas x 2 threads (no GPU); replicas on CPUs 0-1 | 2-3 | 4-5 | 6-7 (one physical core each); gateway, controller and load generator on CPUs 8-15; readiness hold T = 30 s: injected load delay (emulates network model load); inference is real; CPU Intel(R) Core(TM) i5-14450HX (16 logical CPUs), 2026-10-27**

Scenario `real`: 240 s; noisy tenant C spikes from 90 s to 170 s; victims A, B. Every setup replays the same trace in real time (seed 1..5 for run k = 0..4, the same for every setup), one setup at a time on a fresh pool: 2 replicas started and ready before traffic (the static partition: all 4), budget 4 replicas, 4 slots per replica, control cycle 10 s, 20s cooldown between runs. Every control loop makes its first sync a seeded delay in [0, cycle) after its stack starts (the same delay for every setup of a run index; listed per run below), so runs sample the phase of the control cycle relative to the spike. Latencies are measured by the load generator (wall clock, streaming).

## Main results (mean ± 95% CI over runs)

| metric | S2-keda-queue (N=5) | S3-static-partition (N=5) | S4-flowcontrol-equiv (N=5) | S5-ts-no-gateway (N=5) | S7-ts-full (N=5) |
| --- | --- | --- | --- | --- | --- |
| victim_violation_pct | 91.545 ± 2.681 | 21.908 ± 7.595 | 48.777 ± 19.894 | 88.388 ± 10.447 | 27.336 ± 15.839 |
| victim_shed_aware_violation_pct | 91.545 ± 2.681 | 21.908 ± 7.595 | 50.736 ± 17.018 | 88.388 ± 10.447 | 27.517 ± 16.265 |
| noisy_shed_pct | 0.000 ± 0.000 | 0.000 ± 0.000 | 57.391 ± 4.955 | 0.000 ± 0.000 | 44.873 ± 7.787 |
| reaction_time_s | 9.231 ± 4.169 | n/a | 23.226 ± 10.808 | 9.113 (n=1) | 9.221 ± 4.170 |
| replica_minutes | 12.680 ± 0.139 | 16.000 ± 0.000 | 11.663 ± 1.142 | 15.113 ± 0.678 | 15.073 ± 0.680 |
| jain_index | 0.759 ± 0.206 | 0.619 ± 0.081 | 0.913 ± 0.051 | 0.704 ± 0.208 | 0.953 ± 0.043 |
| max_counter_gap | n/a | n/a | n/a | n/a | 489.682 ± 293.866 |
| reclaim_latency_s | n/a | n/a | n/a | n/a | n/a |
| policy_latency_mean_ms | n/a | n/a | n/a | n/a | 0.158 ± 0.071 |
| p99_ttft_s_A | 93.512 ± 15.256 | 1.859 ± 0.619 | 5.145 ± 1.982 | 56.552 ± 16.592 | 3.535 ± 1.799 |
| p99_tpot_s_A | 0.171 ± 0.025 | 0.093 ± 0.039 | 0.148 ± 0.009 | 0.171 ± 0.024 | 0.149 ± 0.018 |
| p99_ttft_s_B | 92.572 ± 16.537 | 8.278 ± 3.038 | 7.073 ± 2.736 | 55.452 ± 17.787 | 6.379 ± 3.953 |
| p99_tpot_s_B | 0.166 ± 0.010 | 0.149 ± 0.033 | 0.142 ± 0.011 | 0.159 ± 0.025 | 0.161 ± 0.028 |
| p99_ttft_s_C | 94.998 ± 16.322 | 121.052 ± 0.192 | 20.861 ± 4.613 | 56.541 ± 15.939 | 13.377 ± 6.649 |
| p99_tpot_s_C | 0.175 ± 0.017 | 0.155 ± 0.009 | 0.144 ± 0.004 | 0.176 ± 0.017 | 0.166 ± 0.023 |
| completed | 421.600 ± 26.025 | 304.000 ± 16.473 | 255.800 ± 16.007 | 421.600 ± 26.025 | 292.800 ± 26.389 |
| shed | 0.000 ± 0.000 | 0.000 ± 0.000 | 165.800 ± 18.818 | 0.000 ± 0.000 | 128.800 ± 25.102 |
| failed | 0.000 ± 0.000 | 117.600 ± 16.012 | 0.000 ± 0.000 | 0.000 ± 0.000 | 0.000 ± 0.000 |

`victim_violation_pct`: share of the victims' requests arriving during the spike that missed their TTFT or TPOT SLO (shed requests excluded, failed ones count as misses); `victim_shed_aware_violation_pct` also counts shed requests as misses; `noisy_shed_pct`: share of the noisy tenant's requests rejected with 429; `reaction_time_s`: first protective action (scale-up or newly applied shedding) after the spike starts, which includes the wait for the next control cycle (its phase is sampled per run, see above); `replica_minutes`: integral of ready + starting replicas over the scenario; `jain_index`: Jain's fairness of SLO attainment / target during the spike; `max_counter_gap`: largest virtual-token-counter gap between backlogged tenants (fair gateway only); `policy_latency_mean_ms`: controller-to-gateway policy propagation; `p99_*`: per-tenant p99 over completed requests (seconds); n/a = not defined for that setup.

![Victim SLO violation by setup, mean with 95% CI](victim_violation.svg)

![Ready replicas over time, run 0](timeline_replicas.svg)

![Victim p99 TTFT over time, run 0](timeline_victim_ttft.svg)

## Pre-registered claim: S7 vs S4 (README.md)

Victim (A, B) SLO attainment during C's spike = 100 - victim_violation_pct, paired by run index (same trace); readiness hold T = 30 s: injected load delay (emulates network model load); inference is real. Decision rule: supported if the 95% CI of the mean paired difference S7 - S4 is wholly above 0.

| run | S7 attainment % | S4 attainment % | S7 - S4 | S7 shed-aware % | S4 shed-aware % | C shed % S7 | C shed % S4 | replica-min S7 | replica-min S4 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 0 | 77.778 | 53.333 | 24.444 | 77.778 | 53.333 | 43.446 | 57.303 | 15.833 | 12.333 |
| 1 | 88.571 | 78.125 | 10.446 | 88.571 | 71.429 | 36.396 | 50.530 | 15.000 | 10.150 |
| 2 | 53.448 | 41.379 | 12.069 | 52.542 | 40.678 | 45.578 | 59.184 | 15.200 | 12.367 |
| 3 | 71.111 | 38.636 | 32.475 | 71.111 | 37.778 | 44.966 | 59.732 | 14.300 | 11.467 |
| 4 | 72.414 | 44.643 | 27.771 | 72.414 | 43.103 | 53.979 | 60.208 | 15.034 | 12.000 |

Mean paired difference S7 - S4: **21.441 percentage points**, 95% CI [9.346, 33.536], N = 5 pairs. **Verdict: supported.**

## Setups

| setup | gateway | control plane |
| --- | --- | --- |
| S2-keda-queue | passthrough | KEDA (scalerun) on queued requests: waiting inside the replicas + gateway queue, 4 per replica |
| S3-static-partition | partition | none: pool fixed at capacity, guaranteed replicas as each tenant's slice |
| S4-flowcontrol-equiv | roundrobin | KEDA (scalerun) on the gateway queue length, 4 per replica |
| S5-ts-no-gateway | passthrough | TenantScaler controller, no gateway policy (coupling disabled) |
| S7-ts-full | fair | TenantScaler controller (arbiter + stabilizer) pushing policy to the fair gateway |

KEDA (S2/S4) uses the Kubernetes defaults of internal/autoscale: 4 queued requests per replica, activation 0, scale-down window 300 s, at most max(4, current) replicas added per sync.

## Runs

Every run, in the order it ran (`#` = sequence; `order` = position in its run index's seeded setup order, decision Q14). CPU % is the effective frequency as a percentage of nominal when traffic started and ended.

| # | setup | run | order | seed | victim violation % | victim shed-aware violation % | noisy shed % | requests | completed | shed | failed | failure causes | warm replicas | stack start s | control phase s | replicas started | mean cold start s | drain timed out | CPU % start/end | tokenizer fallbacks |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | S2-keda-queue | 0 | 1 | 1 | 88.889 | 88.889 | 0.000 | 407 | 407 | 0 | 0 | - | 2 | 30.022 | 5.729 | 4 | 30.000 | false | 69.503 / 90.705 | 0 |
| 2 | S3-static-partition | 0 | 2 | 1 | 17.778 | 17.778 | 0.000 | 407 | 297 | 0 | 110 | http 503 Service Unavailable: 110 | 4 | 30.036 | - | 4 | 30.000 | false | 97.740 / 92.260 | 0 |
| 3 | S4-flowcontrol-equiv | 0 | 3 | 1 | 46.667 | 46.667 | 57.303 | 407 | 254 | 153 | 0 | - | 2 | 30.016 | 5.729 | 4 | 30.000 | false | 90.617 / 99.871 | 0 |
| 4 | S5-ts-no-gateway | 0 | 4 | 1 | 75.556 | 75.556 | 0.000 | 407 | 407 | 0 | 0 | - | 2 | 30.023 | 5.729 | 4 | 30.000 | false | 95.191 / 90.527 | 0 |
| 5 | S7-ts-full | 0 | 5 | 1 | 22.222 | 22.222 | 43.446 | 407 | 291 | 116 | 0 | - | 2 | 30.022 | 5.729 | 4 | 30.000 | false | 100.321 / 109.156 | 0 |
| 6 | S7-ts-full | 1 | 1 | 2 | 11.429 | 11.429 | 36.396 | 400 | 297 | 103 | 0 | - | 2 | 30.023 | 9.084 | 4 | 30.000 | false | 97.827 / 92.712 | 0 |
| 7 | S4-flowcontrol-equiv | 1 | 2 | 2 | 21.875 | 28.571 | 50.530 | 400 | 254 | 146 | 0 | - | 2 | 30.021 | 9.084 | 3 | 30.000 | false | 83.796 / 95.239 | 0 |
| 8 | S3-static-partition | 1 | 3 | 2 | 17.143 | 17.143 | 0.000 | 400 | 301 | 0 | 99 | http 503 Service Unavailable: 99 | 4 | 30.034 | - | 4 | 30.000 | false | 87.631 / 89.180 | 0 |
| 9 | S2-keda-queue | 1 | 4 | 2 | 91.429 | 91.429 | 0.000 | 400 | 400 | 0 | 0 | - | 2 | 30.024 | 9.084 | 4 | 30.000 | false | 80.893 / 95.792 | 0 |
| 10 | S5-ts-no-gateway | 1 | 5 | 2 | 91.429 | 91.429 | 0.000 | 400 | 400 | 0 | 0 | - | 2 | 30.021 | 9.084 | 4 | 30.000 | false | 113.300 / 85.616 | 0 |
| 11 | S7-ts-full | 2 | 1 | 3 | 46.552 | 47.458 | 45.578 | 449 | 314 | 135 | 0 | - | 2 | 30.025 | 0.128 | 4 | 30.000 | false | 100.583 / 101.167 | 0 |
| 12 | S4-flowcontrol-equiv | 2 | 2 | 3 | 58.621 | 59.322 | 59.184 | 449 | 274 | 175 | 0 | - | 2 | 30.020 | 0.128 | 4 | 30.000 | false | 69.572 / 107.059 | 0 |
| 13 | S5-ts-no-gateway | 2 | 3 | 3 | 94.915 | 94.915 | 0.000 | 449 | 449 | 0 | 0 | - | 2 | 30.019 | 0.128 | 4 | 30.000 | false | 95.637 / 89.715 | 0 |
| 14 | S3-static-partition | 2 | 4 | 3 | 32.203 | 32.203 | 0.000 | 449 | 325 | 0 | 124 | http 503 Service Unavailable: 124 | 4 | 30.049 | - | 4 | 30.000 | false | 89.079 / 101.717 | 0 |
| 15 | S2-keda-queue | 2 | 5 | 3 | 94.915 | 94.915 | 0.000 | 449 | 449 | 0 | 0 | - | 2 | 30.011 | 0.128 | 4 | 30.000 | false | 86.324 / 92.436 | 0 |
| 16 | S4-flowcontrol-equiv | 3 | 1 | 4 | 61.364 | 62.222 | 59.732 | 438 | 259 | 179 | 0 | - | 2 | 30.019 | 6.552 | 4 | 30.000 | false | 83.581 / 101.899 | 0 |
| 17 | S7-ts-full | 3 | 2 | 4 | 28.889 | 28.889 | 44.966 | 438 | 304 | 134 | 0 | - | 2 | 30.018 | 6.552 | 4 | 30.000 | false | 106.986 / 101.263 | 0 |
| 18 | S2-keda-queue | 3 | 3 | 4 | 91.111 | 91.111 | 0.000 | 438 | 438 | 0 | 0 | - | 2 | 30.019 | 6.552 | 4 | 30.000 | false | 73.923 / 76.492 | 0 |
| 19 | S5-ts-no-gateway | 3 | 4 | 4 | 95.556 | 95.556 | 0.000 | 438 | 438 | 0 | 0 | - | 2 | 30.018 | 6.552 | 4 | 30.000 | false | 93.082 / 77.156 | 0 |
| 20 | S3-static-partition | 3 | 5 | 4 | 20.000 | 20.000 | 0.000 | 438 | 307 | 0 | 131 | http 503 Service Unavailable: 131 | 4 | 30.033 | - | 4 | 30.000 | false | 65.238 / 103.678 | 0 |
| 21 | S7-ts-full | 4 | 1 | 5 | 27.586 | 27.586 | 53.979 | 414 | 258 | 156 | 0 | - | 2 | 30.012 | 0.241 | 5 | 30.000 | false | 98.457 / 138.036 | 0 |
| 22 | S3-static-partition | 4 | 2 | 5 | 22.414 | 22.414 | 0.000 | 414 | 290 | 0 | 124 | http 503 Service Unavailable: 124 | 4 | 30.033 | - | 4 | 30.000 | false | 78.092 / 115.203 | 0 |
| 23 | S2-keda-queue | 4 | 3 | 5 | 91.379 | 91.379 | 0.000 | 414 | 414 | 0 | 0 | - | 2 | 30.018 | 0.241 | 4 | 30.000 | false | 71.867 / 72.283 | 0 |
| 24 | S4-flowcontrol-equiv | 4 | 4 | 5 | 55.357 | 56.897 | 60.208 | 414 | 238 | 176 | 0 | - | 2 | 30.020 | 0.241 | 4 | 30.000 | false | 85.075 / 77.126 | 0 |
| 25 | S5-ts-no-gateway | 4 | 5 | 5 | 84.483 | 84.483 | 0.000 | 414 | 414 | 0 | 0 | - | 2 | 30.026 | 0.241 | 5 | 30.000 | false | 99.730 / 78.452 | 0 |

## Drift check (first vs last run of each setup)

Each setup's first and last run (by sequence) on the main metric and the CPU frequency. They use different seeds (different traces), so a change here mixes drift and trace; a change in the same direction for every setup points to drift of the machine.

| setup | first run (#) | last run (#) | victim violation % first | last | change | CPU % first | CPU % last |
| --- | --- | --- | --- | --- | --- | --- | --- |
| S2-keda-queue | run 0 (#1) | run 4 (#23) | 88.889 | 91.379 | 2.490 | 69.503 | 71.867 |
| S3-static-partition | run 0 (#2) | run 4 (#22) | 17.778 | 22.414 | 4.636 | 97.740 | 78.092 |
| S4-flowcontrol-equiv | run 0 (#3) | run 4 (#24) | 46.667 | 55.357 | 8.690 | 90.617 | 85.075 |
| S5-ts-no-gateway | run 0 (#4) | run 4 (#25) | 75.556 | 84.483 | 8.927 | 95.191 | 99.730 |
| S7-ts-full | run 0 (#5) | run 4 (#21) | 22.222 | 27.586 | 5.364 | 100.321 | 98.457 |

Per-run raw data: `records/<setup>-run<k>.csv` (one row per request), `timeline/<setup>-run<k>.csv` (1 s samples), `decisions/<setup>-run<k>.jsonl` (controller setups: every decision with its reason); all metrics per run in `runs.csv`, everything in `summary.json`.

## What was NOT measured

- Setups S1-cpu-hpa, S6-ts-no-borrowing, S7F-ts-full-forecast, S8-ts-naive-demand, S4b-flowcontrol-outstanding, S7nr-ts-no-reservation: not part of these real runs; the SIMULATED evaluation (`tenantscaler experiment`) covers every setup.
- CPU temperature: not readable without administrator rights (the ACPI thermal zones Windows exposes are not CPU sensors); the CPU's effective frequency is logged per run instead.
- Model load time: each replica's readiness is held 30 s after launch, an injected load delay (emulates network model load); inference is real; the replicas' own load takes well under a second.
- GPU inference: replicas run on CPU cores only (backend.llama.gpuLayers 0, enforced; PLAN.md decision Q8); a GPU pool would behave differently.
- Cold-start sensitivity: the replicas' cold start was not varied (mean 30.000 s over the 101 replicas the runs started); the cold-start sweep exists only in the simulated evaluation.
- Kubernetes: the pool is a local process manager, not a Deployment; KEDA and the controller act on it directly.
- The demand forecaster (S7F) is off in every real setup.
- Isolation of the measurement: the load generator, gateway and controller share this machine with the replicas (they are not pinned; replicas are).
- Tokens: the gateway and controller count prompt words, the replica's tokenizer counts tokens (see results/real/profile.md for the ratio).
- Reclaim latency: the scenario has no returning owner (n/a).
- Per-tenant queues inside the replicas: with the passthrough gateway, requests waiting inside a replica count as in flight in the timeline (replicas do not report their queue per tenant).
