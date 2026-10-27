# Real llama.cpp pods on k3s, KEDA baseline beside TenantScaler on mirrored traffic (2026-10-28)

**Setup.** One k3s v1.36.5 node in WSL2 on the laptop.

- **Pods.** Both pools run **real llama.cpp** pods:
  - official server image, build b11913, pinned by digest;
  - Qwen2.5-0.5B-Instruct Q4_K_M, the pinned GGUF with its SHA-256 checked;
  - each pod has 2 CPUs (request and limit), `-t 2 -np 4`, 1024 tokens per slot, prompt cache off.
- **TenantScaler.** Our controller (`-capacity 2`, C = 117, 10 s cycle) and the fair gateway (2 requests in flight per replica). Tenants:
  - a and b: guaranteed 1, max 2;
  - c: guaranteed 0, max 2, priority 50.
- **Baseline.** Real KEDA 2.21 scales its own pool, 1 to 2 replicas, with the metrics-api scaler on its passthrough gateway's `/stats` (`inFlight`, 8 per replica = 4 slots + 4 waiting, 15 s polling, 300 s scale-down).
- **Budget.** 2 + 2 replicas of 2 threads is the laptop's 8-thread safety cap.
- **Traffic.** `tenantscaler mirror` sends every request to TenantScaler, whose answer the client gets, and a copy to the baseline, so both get the same traffic.
- **Load.** `tenantscaler loadgen` from the laptop:
  - a steady and b periodic, 100 s;
  - from +20 s, two c spike loadgens (spike 60 s, run 75 s).
- **Runbook.** `deploy/k3s/README.md`.

**What these numbers are.** They come from single short runs, inside a VM, unpinned, on a budget of 2. They are not comparable with `results/real/`, where replicas are pinned to P-cores on bare metal with build b11898. They are not headline results.

The 2 s TTFT SLO was derived on bare metal (`results/real/scaling.md`). Here it is only a reference line. The C value of 117 is also the bare-metal measurement, used as a placeholder.

## Run 1: mirror on Windows through `kubectl port-forward`

Loadgen (`run1-loadgen-*.txt`):

| Tenant | Answered | 429 | Failed |
|---|---|---|---|
| a | 25 | 0 | 3 |
| b | 17 | 0 | 2 |
| c (2 loadgens) | 21 + 22 | 147 + 157 | 1 |

The failures are listed in `loadgen-warnings.txt`. There were two kinds:
- a and b each got one `502`, when the port-forward between the Windows mirror and the cluster dropped (`EOF`, then refused). This is why run 2 uses a mirror on the node.
- The rest are `aborted`: loadgen stopped mid-request at the end of `-duration`.

Gateways' windowed statistics, last 156 s:

| Tenant | TenantScaler | Baseline (KEDA) |
|---|---|---|
| a | 26 completed, 0 shed, SLO met 65.4%, p99 TTFT 3.85 s | 4 completed, 50.0%, p99 8.78 s |
| b | 18 completed, 0 shed, 88.9%, p99 3.65 s | 11 completed, 45.5%, p99 78.17 s |
| c | 43 completed, 304 shed (429), 86.0%, p99 3.66 s | 134 completed, 0 shed, 4.5%, p99 83.92 s |

**Load during c's spike:**
- Windows total CPU was 47, 55 and 56% (three 2 s samples), in the `vmmemWSL` process.
- `kubectl top pods`: TenantScaler pods 753m and 1214m, baseline pods 1617m and 1319m. All four pods were running, two per pool.
- TenantScalers: c's demand was 14 replicas, its grant 1, and `shedding: true`.

## Run 2: mirror on the node, to the gateways' ClusterIPs, no port-forward

The baseline started with 2 warm replicas, left from run 1, while TenantScaler started with 1. This favours the baseline.

Loadgen (`run2-loadgen-*.txt`):

| Tenant | Answered | 429 | Failed |
|---|---|---|---|
| a | 32 | 0 | 2 |
| b | 28 | 1 | 6 |
| c (2 loadgens) | 8 + 20 | 219 + 154 | 1 |

There were no `502`s. All failures were `aborted` or `context deadline exceeded` at the end of `-duration`.

Gateways, last 120 s:

| Tenant | TenantScaler | Baseline (KEDA) |
|---|---|---|
| a | 32 completed, 0 shed, SLO met 53.1%, p99 TTFT 6.49 s | 8 completed, 37.5%, p99 57.48 s |
| b | 28 completed, 1 shed, 35.7%, p99 6.49 s | 10 completed, 30.0%, p99 59.85 s |
| c | 28 completed, 373 shed (429), 0%, p99 61.39 s | 89 completed, 0 shed, 4.5%, p99 60.20 s |

Windows total CPU during the spike was 53, 53 and 54%.

## Run 3: a's steady traffic alone, 60 s, right after run 2

| | TenantScaler | Baseline (KEDA) |
|---|---|---|
| a | 14 completed, SLO met 100%, p99 TTFT 1.05 s | 9 completed, 0%, p99 122.86 s |

- Without a spike, a replica in this VM answers within the SLO.
- The baseline was still working through c's backlog from run 2, so a waited behind it.
- Windows CPU was still 40, 50 and 46%.

## Reading

- On identical traffic, TenantScaler shed the noisy tenant c with 429s and served a and b 3 to 4 times more requests than the KEDA baseline. Their p99 TTFT was about 6.5 s against about 58 s in run 2.
- The KEDA baseline has no notion of tenants. c's backlog delayed everyone, even after the spike ended.
- a and b did **not** meet the 2 s SLO under c's spike in this setup. Two likely causes, neither measured separately:
  - On a budget of 2, c borrows b's guaranteed replica while b is idle. When b returns it waits for c's running requests, which are never preempted.
  - Four pods share unpinned hyperthreads.

## Files

| File | Contents |
|---|---|
| `run1-loadgen-*.txt`, `run2-loadgen-*.txt`, `run3-loadgen-a-only.txt` | loadgen output |
| `loadgen-warnings.txt` | each failure category's first occurrence |
| `cluster.txt` | versions, images, Deployments, pods, `kubectl top`, KEDA, TenantScalers, llama args and resources, and the model's SHA-256 |
| `llama-pod-logs.txt` | llama-server start-up: model loaded, 2 threads, 4 slots of 1024 |
| `controller-log.txt`, `gateway-log.txt`, `gateway-baseline-log.txt`, `keda-operator-log.txt` | logs |

The WSL VM restarted after these runs (see `cluster.txt`, pods with one restart each). Those logs therefore only cover the time after it.
