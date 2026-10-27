# Replica profile

**REAL inference profile: llama.cpp b11898-8b28c2394, model qwen2.5-0.5b-instruct-q4_k_m.gguf, replicas with 2 threads (no GPU), 4 slots; replicas on CPUs 0-1 | 2-3 | 4-5 | 6-7 (one physical core each); gateway, controller and load generator on CPUs 8-15; CPU Intel(R) Core(TM) i5-14450HX (16 logical CPUs); 2026-10-27**

One replica, driven directly (no gateway) by closed-loop clients: at concurrency c, c clients each send their next request as soon as the previous one finishes. Every prompt is unique (no prompt-cache hits). Lengths: prompt words log-normal median 80 (sigma 0.4, 16-160), output tokens median 48 (sigma 0.3, 8-96), with ignore_eos so every answer has exactly its max_tokens. Each level runs 5s of warm-up and 1m0s of measurement. Latency percentiles use the requests that arrived and finished inside the measured window (the latency sample); throughput is the work of every request that finished inside the window, whatever its arrival, divided by the window (unbiased for a closed loop in steady state). Weighted tokens = prompt words + 2 x output tokens (the gateway's and controller's accounting); prompt tokens/word is the server tokenizer's count over the prompt words.

- Cold start (launch to first healthy response): **605ms**
- Prefill probe, concurrency 1: 16 prompt words (16.0 server tokens): median TTFT 0.141 s over 8 requests
- Prefill probe, concurrency 1: 160 prompt words (160.2 server tokens): median TTFT 1.280 s over 8 requests

## Concurrency levels

| concurrency | latency sample | completed in window | failed | p50 TTFT s | p99 TTFT s | p50 TPOT ms | p99 TPOT ms | output tok/s | weighted tok/s | prompt tokens/word | meets SLO |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 34 | 35 | 0 | 0.676 | 1.301 | 20.966 | 22.169 | 28.850 | 107.750 | 1.002 | true |
| 2 | 36 | 38 | 0 | 0.702 | 1.318 | 51.190 | 84.734 | 30.800 | 115.183 | 1.000 | true |
| 3 | 35 | 38 | 0 | 0.777 | 1.370 | 79.276 | 106.737 | 31.117 | 115.967 | 1.002 | true |
| 4 | 40 | 44 | 0 | 0.813 | 1.816 | 91.093 | 124.576 | 37.400 | 135.917 | 1.001 | true |
| 5 | 39 | 44 | 0 | 2.058 | 4.422 | 92.477 | 134.103 | 37.400 | 135.917 | 1.002 | false |
| 6 | 40 | 46 | 0 | 3.296 | 5.462 | 95.975 | 123.576 | 38.583 | 139.917 | 1.002 | false |

## Derived

C = 136 weighted tokens/s per replica (concurrency 4, the highest level meeting p99 TTFT <= 2s and p99 TPOT <= 150ms); the config uses 134.

Simulated-engine parameters calibrated from this replica: prefill = overhead + per-word slope fitted to the two prefill probes; interTokenLatency = median TPOT at concurrency 1; timeFactorUnderLoad = least-squares load factor (below); maxNumSeqs = slots; startupDuration = the measured cold start; jitterFraction is kept from the config (not measured).

Load factors: from concurrency 1 to 4 the measured median TTFT grew 1.202 x and the median TPOT 4.345 x. Fitted by least squares over concurrency 2..4: F = 1.202 for prefill (used: prefillFactorUnderLoad 1.2) and 4.651 for decode (used: timeFactorUnderLoad 4.65); one factor for both would be 2.927.

```yaml
controller:
  replicaCapacityTokensPerSec: 136
engine:
  prefillOverhead: 14.587ms
  prefillPerToken: 7.911ms
  interTokenLatency: 20.966ms
  maxNumSeqs: 4
  timeFactorUnderLoad: 4.65
  prefillFactorUnderLoad: 1.2
  startupDuration: 605ms
  jitterFraction: 0.05
```

## Calibration check

Calibration check: the SIMULATED replica running these parameters does NOT reproduce the measurement (±25% on p50 TTFT, p50 TPOT and weighted tokens/s, and the same SLO verdict, at every level); 1 deviations: c=6 p50 TTFT: simulated 4.203 s vs measured 3.296 s (+28%).

measured / SIMULATED (calibrated engine, 20s measured per level after the same warm-up):

| concurrency | p50 TTFT s | p99 TTFT s | p50 TPOT ms | p99 TPOT ms | weighted tok/s | meets SLO |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 0.676 / 0.642 | 1.301 / 1.329 | 20.966 / 21.876 | 22.169 / 22.043 | 107.750 / 101.650 | true / true |
| 2 | 0.702 / 0.736 | 1.318 / 1.347 | 51.190 / 47.410 | 84.734 / 47.575 | 115.183 / 111.650 | true / true |
| 3 | 0.777 / 0.776 | 1.370 / 1.463 | 79.276 / 73.140 | 106.737 / 73.508 | 115.967 / 121.800 | true / true |
| 4 | 0.813 / 0.803 | 1.816 / 1.455 | 91.093 / 98.527 | 124.576 / 99.346 | 135.917 / 128.000 | true / true |
| 5 | 2.058 / 2.323 | 4.422 / 4.608 | 92.477 / 97.818 | 134.103 / 98.781 | 135.917 / 136.050 | false / false |
| 6 | 3.296 / 4.203 | 5.462 / 5.435 | 95.975 / 97.738 | 123.576 / 98.336 | 139.917 / 128.000 | false / false |

Not measured here: more than one replica at a time (replicas are pinned to separate cores, but share memory bandwidth and caches), the gateway's overhead, and GPU inference.
