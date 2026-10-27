# First run on a real Kubernetes API server (k3s smoke test, 2026-10-28)

**What this shows.** The manifests in `deploy/k8s/` ran unchanged in sizing on a real cluster:
- the controller scaled the `model-pool` Deployment up and down through `deployments/scale`;
- it wrote every tenant's decision into the `TenantScaler` status (CRD, status subresource, RBAC);
- the gateway followed the pods through EndpointSlices;
- keys came from a Kubernetes Secret.

**What it does not show.** The replicas are the **SIMULATED** engine (`tenantscaler replica`), not llama.cpp, so every latency here is simulated. The cluster is one node inside a WSL2 VM, unpinned. Nothing here is comparable with `results/real/`, and nothing here is a headline result.

## Setup

| | |
|---|---|
| Cluster | k3s v1.36.5+k3s1, one node, containerd 2.3.4-k3s1.36, see `cluster.txt` |
| Host | WSL2 (kernel 6.18.54.1), Ubuntu 24.04.5, mirrored networking, on the Windows laptop |
| Image | `tenantscaler:dev`, 32 MB, built without Docker by `scripts/k3s/image.sh` from `0.1.0+358a324` (Go code identical to branch `nilay-live-demo`) |
| Manifests | `deploy/k8s/` of branch `nilay-k8s`: budget 10 replicas; tenants a, b, c with guarantee 3, max 8, priority 100; c's key is capped at sheddable |
| Deploy | `scripts/k3s/up.sh` |
| Load | From the node to the gateway's ClusterIP, with no port-forward. `tenantscaler loadgen`: a steady and b periodic for 200 s; from +40 s, three c spike loadgens (spike 90 s, run 120 s) |

## Results

Pool size from `timeline.txt` (5 s samples) and the controller log, times UTC:

| Time | Pool (spec/ready) | Why |
|---|---|---|
| 20:19:19 | 1/1 | no traffic yet |
| 20:19:44 | 2/1 | a and b each granted 1 |
| 20:20:14 | 6/2 | c's spike: c granted 4 |
| 20:20:50 | 6/6 | new pods ready about 36 s later (simulated cold start) |
| 20:21:01, 20:21:20 | 7, then 8 | c granted 5, then 6 |
| 20:22:37 | 8/8, desired 1 | traffic over; the scale-down waits out its stabilization window |
| 20:23:41 → 20:25:01 | 8 → 5 → 3 → 2 → 1 | stepped scale-down |

The gateway logged `endpoints changed` for every scale-up step (`gateway-log.txt`).

Per tenant, from the gateway's counters (`gateway-metrics.txt`):

| Tenant | Arrivals | 429 | Completed, SLO met | Completed, SLO missed | Failed |
|---|---|---|---|---|---|
| a (steady) | 62 | 0 | 62 | 0 | 0 |
| b (periodic) | 48 | 0 | 48 | 0 | 0 |
| c (3 x spike) | 748 | 98 (all `deadline-unreachable`) | 642 | 7 | 1 (`canceled`: loadgen stopped mid-stream) |

Why `shedding` stayed false: c's demand peaked at 6 replicas, below its max of 8 and within the budget of 10, so the controller never turned on over-share shedding (`over-share` = 0). The 98 rejections came during the scale-up lag: c's queue was too long for its requests to start within their 2 s deadline, so the gateway rejected them at once instead of making them wait. a and b were never rejected.

Access control (`curl` from the node): `/metrics` without the admin key, `POST /policy` without the admin key, and a completion without a key all answered 401. The RBAC checks in `cluster.txt` show the controller can update TenantScaler status and `deployments/scale`, while the gateway can list EndpointSlices but cannot scale.

## Files

| File | Contents |
|---|---|
| `timeline.txt` | pool and per-tenant grant, demand and shedding every 5 s |
| `a.txt`, `b.txt`, `c1.txt`-`c3.txt` | loadgen output |
| `gateway-metrics.txt` | arrivals, 429s by reason, completions, failures |
| `tenantscalers.yaml` | the TenantScalers at the end, with status |
| `decisions.json` | the controller's decision log (`/decisions`) |
| `controller-log.txt`, `gateway-log.txt` | pod logs |
| `cluster.txt` | versions, node, image, Deployments, EndpointSlices, RBAC checks |
