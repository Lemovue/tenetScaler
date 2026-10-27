# k3s run with the laptop's demo sizing: the noisy neighbour is shed (2026-10-28)

This is the same cluster, image and load as `../2026-10-28-k3s-smoke/`, after `scripts/k3s/demo-size.sh`:
- budget 4 replicas (controller `-capacity 4`);
- tenants a, b and c each guaranteed 1 and capped at 3;
- c at priority 50, a and b at 100.

That is the sizing of `config/live-demo.yaml`.

The replicas are the **SIMULATED** engine, so latencies are simulated. The cluster is one k3s node in a WSL2 VM, and this is a smoke test, not a headline result.

## Results

From `timeline.txt` (5 s samples, UTC):
- a and b got 1 replica each as soon as their traffic started (20:30:36).
- c's spike from 20:31:27 on wanted 4 to 7 replicas, above its cap of 3. The controller granted c 2 or 3 (whatever a and b left of the 4) and set **`shedding: true` for c** until 20:32:57. a and b were never shed.
- The pool reached the budget, 4/4 ready at 20:31:58, and never exceeded it.

The gateway's counters are cumulative since the gateway pod started, which was before the first smoke run. `demo-size.sh` restarted only the controller. This run's numbers are therefore `gateway-metrics.txt` minus the first run's `../2026-10-28-k3s-smoke/gateway-metrics.txt`:

| Tenant | Arrivals | 429 | Completed, SLO met | Completed, SLO missed | Failed |
|---|---|---|---|---|---|
| a (steady) | 49 | 0 | 49 | 0 | 0 |
| b (periodic) | 67 | 0 | 67 | 0 | 0 |
| c (3 x spike) | 840 | 219: 188 `over-share`, 31 `deadline-unreachable` | 620 | 0 | 1 (`canceled`: loadgen stopped mid-stream) |

The loadgen outputs agree:
- a: 49 answered.
- b: 67 answered.
- c: 219+210+191 = 620 answered, 72+65+82 = 219 rejected, and 1 failed.

`over-share` is the controller's shedding at work: c asked for more than its cap while the pool was full, so its sheddable requests were refused at once instead of slowing a and b.

`gateway-log.txt` also holds the first run's scale-down as seen by EndpointSlice discovery: endpoints went from 8 to 5, 3, 2 and 1 between 20:23:42 and 20:25:03. In this run they went from 1 to 2 to 4.

## Files

The files are the same as in `../2026-10-28-k3s-smoke/`. `tenantscalers.yaml` shows the demo specs (guaranteed 1, max 3, c at priority 50), and `controller-log.txt` shows the controller started with a pool of 4 replicas.
