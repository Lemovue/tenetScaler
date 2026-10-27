# Replica scaling curve

**REAL inference: Intel(R) Core(TM) i5-14450HX (16 logical CPUs), 2026-10-27**

k replicas loaded at once, each closed-loop at 4 concurrent requests (the dispatch window), unique prompts, output length forced to max_tokens; 5s warm-up, 1m0s measured per level. Throughput is the sum over replicas, percentiles are pooled. Per-replica rows: scaling.csv.

| replicas | requests | weighted tok/s (total) | per replica (mean) | per replica (min) | efficiency vs 1 replica | p50 TTFT s | p99 TTFT s | p50 TPOT ms | p99 TPOT ms | failed | CPU performance % of nominal |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 39 | 133.567 | 133.567 | 133.567 | 1.000 | 0.810 | 1.972 | 91.873 | 133.865 | 0 | 115.899 |
| 2 | 77 | 261.333 | 130.667 | 129.333 | 0.978 | 0.816 | 1.845 | 95.491 | 137.128 | 0 | 135.414 |
| 3 | 112 | 371.383 | 123.794 | 121.383 | 0.927 | 0.848 | 2.072 | 101.502 | 134.867 | 0 | 170.496 |
| 4 | 137 | 466.700 | 116.675 | 114.917 | 0.874 | 0.890 | 1.754 | 106.417 | 156.623 | 0 | 171.930 |

## SLO and capacity derivation (rules: docs/DESIGN.md section 20)

With all 4 replicas loaded at the dispatch window: pooled p99 TPOT 156.623 ms, pooled p99 TTFT 1.754 s.
TPOT SLO = 1.25 x 156.623 ms, rounded up to 10 ms = 200 ms.
C = pooled weighted tokens/s / 4 = 117 weighted tokens/s per replica.
TTFT SLO = 2 s: the full budget meets it (pooled p99 TTFT 1.754 s).
