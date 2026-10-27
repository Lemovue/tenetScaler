# Demand forecaster evaluation (simulated workload)

> **SIMULATED:** the series are synthetic traces from `workload.Generate`, and the control-loop effect comes from the discrete-event simulator (`internal/sim`) with the simulated engine latency model.

Forecaster: additive Holt-Winters (alpha 0.5, beta 0.1, gamma 0.3), compared with naive (last value) and seasonal-naive (value one season earlier). Series: per-bucket arrival demand in weighted tokens/s (input + 2 x max output tokens, what the controller observes); bucket = one control cycle = 15s; season = 8 buckets (the periodic tenant's period). Each series has 160 buckets (2400s): the first 96 (60%) train, the remaining 64 are held out. Seeds [1 2 3 4 5 6 7 8 9 10]; tables show mean ± 95% CI over seeds.

## Leakage statement

- The split is chronological: a model observes only the training buckets before it is scored; nothing is shuffled.
- Scoring is rolling one-step-ahead (`forecast.Evaluate`): each held-out bucket t is forecast BEFORE the model observes it, so a prediction uses only buckets < t. A spy-series unit test in `internal/forecast` checks this.
- Hyperparameters were fixed a priori and the season comes from the scenario's declared period; nothing was tuned on held-out buckets.
- Every seed is an independently generated trace.
- For the failure analysis the spike tenant's spike is moved into the held-out part (start 1800s), so no model sees it during training.

## Accuracy: periodic tenant B (held-out, rolling one-step)

| method | MAPE % | sMAPE % | MAE (weighted tok/s) |
| --- | --- | --- | --- |
| naive | 65.663 ± 11.365 | 50.062 ± 3.443 | 376.467 ± 20.280 |
| seasonal-naive | 49.576 ± 7.174 | 41.174 ± 3.305 | 294.886 ± 26.039 |
| holt-winters | 47.691 ± 7.112 | 38.798 ± 3.056 | 273.346 ± 16.325 |

Holt-Winters MAPE is not distinguishable (95% CIs overlap) from seasonal-naive (47.691 vs 49.576).

## Failure analysis: spike tenant C

An unannounced spike has no history to learn from. Ratios are one-step forecast / actual; 1 is perfect, below 1 under-forecasts, above 1 over-forecasts.

| method | MAPE % | sMAPE % | MAE (weighted tok/s) | forecast/actual at onset | forecast/actual after the spike |
| --- | --- | --- | --- | --- | --- |
| naive | 78.534 ± 13.460 | 52.373 ± 3.838 | 464.627 ± 38.613 | 0.216 ± 0.081 | 5.274 ± 2.035 |
| seasonal-naive | 166.577 ± 39.898 | 74.281 ± 4.488 | 890.035 ± 46.450 | 0.292 ± 0.149 | 5.814 ± 1.633 |
| holt-winters | 80.736 ± 12.818 | 56.417 ± 4.232 | 481.654 ± 25.694 | 0.229 ± 0.122 | 5.637 ± 1.865 |

- At the onset Holt-Winters forecasts 0.229 of the actual demand: it does not anticipate the spike. An under-forecast costs nothing extra by construction: the controller acts on max(reactive, forecast), so it falls back on the reactive estimate, which sees the spike at the next control cycle.
- In the first bucket after the spike Holt-Winters forecasts 5.637 of the actual demand: an over-forecast, which would hold replicas the tenant no longer needs. The controller's trust gate (`forecast.Tracker`, 4 scored buckets, distrusts a mean APE above 100%) trusted the forecaster after 37.500 ± 16.326% of the buckets in the season following the spike.

## Effect on the control loop: S7 vs S7F (simulated)

| scenario | setup | B p99 TTFT (s) | B SLO violation % | replica-minutes | cycles forecast raised B | cycles forecast raised C |
| --- | --- | --- | --- | --- | --- | --- |
| default (600s) | S7-ts-full | 0.324 ± 0.014 | 0.000 ± 0.000 | 94.123 ± 1.212 | 0.000 ± 0.000 | 0.000 ± 0.000 |
| default (600s) | S7F-ts-full-forecast | 0.318 ± 0.014 | 0.000 ± 0.000 | 95.125 ± 0.955 | 12.400 ± 1.399 | 8.300 ± 1.014 |
| default-long (2400s) | S7-ts-full | 0.291 ± 0.007 | 0.000 ± 0.000 | 380.612 ± 2.459 | 0.000 ± 0.000 | 0.000 ± 0.000 |
| default-long (2400s) | S7F-ts-full-forecast | 0.287 ± 0.007 | 0.000 ± 0.000 | 394.200 ± 1.236 | 74.500 ± 3.667 | 57.100 ± 3.346 |

- default: with the forecaster, B's SLO violation is not distinguishable (95% CIs overlap) from S7's (0.000% vs 0.000%) and replica-minutes are not distinguishable (95% CIs overlap) from S7's (95.125 vs 94.123). The forecast also raised the spike tenant C's demand in 8.300 cycles; C has no recurring peak, so that capacity was not driven by a real seasonal pattern.
- default-long: with the forecaster, B's SLO violation is not distinguishable (95% CIs overlap) from S7's (0.000% vs 0.000%) and replica-minutes are higher than S7's (394.200 vs 380.612). The forecast also raised the spike tenant C's demand in 57.100 cycles; C has no recurring peak, so that capacity was not driven by a real seasonal pattern.

Holt-Winters needs two full seasons before it forecasts and 4 accurate buckets before it is trusted, so in a short run it acts only in the second half.
