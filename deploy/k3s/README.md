# TenantScaler on a real Kubernetes cluster (k3s in WSL2)

This runs the manifests in `deploy/k8s/` on a real API server:
- the controller scales the `model-pool` Deployment through `deployments/scale`;
- the gateway finds the ready pods through EndpointSlices;
- every tenant's decision lands in its `TenantScaler` status.

The replicas are the **simulated** engine (`tenantscaler replica`), not llama.cpp, so every latency number is SIMULATED. The cluster is one node inside a WSL2 VM, unpinned. Nothing measured here is comparable with `results/real/`. Evidence from the first runs is in `results/k8s/`.

## Once per laptop

1. **WSL2 and Ubuntu 24.04.** In an Administrator PowerShell run `wsl --install`, then reboot. In a normal PowerShell run `wsl --install -d Ubuntu-24.04` and create your Linux user.

2. **`%UserProfile%\.wslconfig`.** Without `instanceIdleTimeout=-1`, WSL stops the VM, and k3s with it, after the last shell closes. Mirrored networking puts WSL on the laptop's own IP.
   ```ini
   [wsl2]
   networkingMode=mirrored

   [general]
   instanceIdleTimeout=-1
   ```
   Then run `wsl --shutdown`.

3. **k3s.** In Ubuntu run the official installer, which verifies the binary's sha256. It is pinned to release v1.36.5+k3s1 (the stable channel's release on 2026-10-28). Traefik is disabled because nothing here uses an Ingress.
   ```bash
   curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='v1.36.5+k3s1' INSTALL_K3S_EXEC='--disable traefik' sh -
   mkdir -p ~/.kube && sudo cat /etc/rancher/k3s/k3s.yaml > ~/.kube/config && chmod 600 ~/.kube/config
   echo 'export KUBECONFIG=~/.kube/config' >> ~/.bashrc && export KUBECONFIG=~/.kube/config
   kubectl get nodes        # Ready
   ```
   The kubeconfig is cluster-admin, so never share it.

## Deploy

1. **Build the binaries.** In Windows PowerShell, from the repository:
   ```powershell
   ./scripts/build-dist.ps1
   ```
   - `dist/tenantscaler-linux-amd64` goes into the image.
   - `dist/tenantscaler-windows-amd64.exe` is what `loadgen.ps1` runs.

2. **Create the key file**, once. It goes outside the repository and the keys are never printed:
   ```powershell
   ./scripts/k3s/keys.ps1
   ```
   - The file is `%UserProfile%\.tenantscaler\k8s.env`.
   - It holds `TS_TENANT_KEYS` with the lower-case tenants `a`, `b`, `c` (the `TenantScaler` names) and `TS_ADMIN_KEY`.
   - The live-demo keys in `.env` use `A/B/C` and do not work here: the gateway refuses keys for tenants it does not know.

3. **Deploy.** In Ubuntu, from the repository:
   ```bash
   sudo bash scripts/k3s/up.sh dist/tenantscaler-linux-amd64 /mnt/c/Users/<you>/.tenantscaler/k8s.env
   ```
   `up.sh` does the following:
   - Builds the image without Docker and imports it into k3s's containerd (`scripts/k3s/image.sh`). The image is one layer: the static binary at `/tenantscaler`, run as UID 65532, with the Dockerfile's entrypoint.
   - Applies the namespace and the CRD.
   - Creates or replaces the Secret `tenantscaler-keys` from the key file.
   - Applies `deploy/k8s/`.
   - Restarts the three Deployments and waits until they are rolled out.

4. **Optional: laptop sizing.** Without it the cluster has the `deploy/k8s` sizing: budget 10, and a, b, c guaranteed 3 with a max of 8. This step sizes it like `config/live-demo.yaml`: budget 4, a, b, c guaranteed 1 with a max of 3, and c at priority 50.
   ```bash
   sudo bash scripts/k3s/demo-size.sh          # undo with --undo; up.sh also resets it
   ```
   c's spike then exceeds its cap: `SHEDDING` turns true for c, and c's requests get 429 with the reason `over-share`.

## Show it

- **Ports.** In Ubuntu, in its own terminal, keep this running:
  ```bash
  bash scripts/k3s/expose.sh
  ```
  - It forwards the gateway to port 8081 on every address and the controller to 127.0.0.1:18090.
  - It reconnects by itself when `up.sh` replaces the pods.
  - In mirrored mode Windows and WSL share ports, so first stop a Windows `demo -live`, which also uses 8081.

- **Dashboard.** The dashboard can watch the cluster instead of starting its own stack. With `expose.sh` running, in Windows PowerShell:
  ```powershell
  bin\tenantscaler.exe demo -config config/live-demo.yaml -observe-gateway http://127.0.0.1:8081 -observe-controller http://127.0.0.1:18090 -observe-keys $env:USERPROFILE\.tenantscaler\k8s.env
  ```
  Then open http://127.0.0.1:8080.
  - The badge reads "KUBERNETES (k3s): SIMULATED replicas".
  - The TenantScaler panel shows:
    - the cluster's ready and desired replicas;
    - the gateway's per-tenant statistics, with the cluster's a, b, c shown as A, B, C;
    - the controller's decision log.
  - The admin key is read from the key file and never printed.
  - Stop only stops watching; the cluster keeps running.

- **Traffic from this laptop.** Use one Windows PowerShell per tenant:
  ```powershell
  ./scripts/k3s/loadgen.ps1 -Tenant a -LoadProfile steady
  ./scripts/k3s/loadgen.ps1 -Tenant b -LoadProfile periodic
  ./scripts/k3s/loadgen.ps1 -Tenant c -LoadProfile spike -Extra '-spike-now', '-spike-duration', '60s'
  ```

- **Watch the cluster decide.** In Ubuntu:
  ```bash
  kubectl -n tenantscaler get tenantscalers -w         # Grant / Demand / Shedding per tenant
  kubectl -n tenantscaler get deployment model-pool -w # replicas set by the controller
  kubectl -n tenantscaler get pods -w
  curl -s 127.0.0.1:18090/decisions                    # why
  kubectl -n tenantscaler logs deploy/gateway | grep "endpoints changed"
  ```

- **Teammates' laptops.** Each one needs their own key: `./scripts/k3s/keys.ps1 -Copy a` puts it on the clipboard. They run:
  ```
  tenantscaler loadgen -gateway http://<laptop IP>:8081 -key <key> -profile ...
  ```
  To keep the key out of shell history, they can set `TS_API_KEY` and leave out `-key`.

  In mirrored mode, inbound LAN traffic to WSL is blocked by the Hyper-V firewall. Opening port 8081 needs an Administrator PowerShell and the laptop owner's approval. The rule has no network-profile limit, so remove it right after the demo.
  ```powershell
  New-NetFirewallHyperVRule -Name TenantScalerGW -DisplayName "TenantScaler gateway (k3s)" -Direction Inbound -VMCreatorId '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}' -Protocol TCP -LocalPorts 8081
  Remove-NetFirewallHyperVRule -Name TenantScalerGW
  ```

## Side by side: the KEDA baseline beside TenantScaler, on the same live traffic

The baseline runs beside TenantScaler in the same cluster:
- it has its own pool of simulated replicas (`model-pool-baseline`);
- a passthrough gateway (`gateway-baseline`) sits in front of that pool;
- real KEDA (2.21) scales the pool from that gateway's `/stats`: 12 requests in flight per replica, which is the simulated engine's 8 running plus the baseline's 4 waiting.

No Prometheus is needed. On Windows, `tenantscaler mirror` serves the teammates on port 8081: TenantScaler answers each request, and the baseline gets a copy of it. Both pools therefore see the same live traffic.

1. **Install KEDA**, once. This is a download, so get the owner's approval first:
   ```bash
   sudo kubectl apply --server-side -f https://github.com/kedacore/keda/releases/download/v2.21.0/keda-2.21.0-core.yaml
   ```
   The core manifest has no admission webhooks, so the operator's `cert-rotation ... keda-admission not found` errors are expected.

2. **Deploy the baseline.** Do this after `up.sh` and `demo-size.sh`:
   ```bash
   sudo kubectl apply -f deploy/k3s/side-by-side/
   ```

3. **Forward to this machine only.** Run this in Ubuntu and keep it running:
   ```bash
   bash scripts/k3s/expose.sh side-by-side
   ```
   It forwards the TenantScaler gateway to 18081, the baseline gateway to 18082 and the controller to 18090.

4. **Start the mirror** in Windows PowerShell. The Windows firewall rule for `bin\tenantscaler.exe` or `scripts/demo/firewall.ps1` covers it, so no Hyper-V rule is needed:
   ```powershell
   bin\tenantscaler.exe mirror -addr 0.0.0.0:8081
   ```

5. **Open the dashboard**, with both panels live:
   ```powershell
   bin\tenantscaler.exe demo -config config/live-demo.yaml -observe-gateway http://127.0.0.1:18081 -observe-controller http://127.0.0.1:18090 -observe-baseline-gateway http://127.0.0.1:18082 -observe-keys $env:USERPROFILE\.tenantscaler\k8s.env
   ```
   KEDA's target is not read, so the baseline panel shows ready replicas only, with no "starting" line.

Teammates run the same `loadgen` command against `http://<laptop IP>:8081` with their cluster key.

A sturdier mirror runs on the node itself, against the gateways' ClusterIPs, so that no port-forward sits in the data path. A port-forward once dropped requests under load (`results/k8s/2026-10-28-k3s-llama-side-by-side/`, run 1). Run it in Ubuntu:

```bash
bash scripts/k3s/mirror.sh dist/tenantscaler-linux-amd64
```

It listens on 8081 with the Hyper-V firewall rule above, so stop the Windows mirror first.

**Narrated backend for the judges.** Run this in Ubuntu:

```bash
python3 scripts/k3s/explain.py --keys /mnt/c/Users/<you>/.tenantscaler/k8s.env
```

It prints in plain words only what changed:

| Line | Shows |
|---|---|
| `CONTROLLER`, `POOL` | each tenant's grant and why, in the controller's own reason text, and the pool size it applies |
| `KEDA` | the baseline's metric and replica target |
| `PODS` | llama pods starting, ready and stopping, in both pools |
| `TRAFFIC` | per tenant every 10 s: req/s, 429s and p99 TTFT at both gateways |

The admin key is read from the file and never printed.

`./scripts/k3s/demo-up.ps1` opens all four windows in one go: expose, the node mirror, the dashboard and `explain.py`. Run it again whenever they were closed. Closing every WSL window can let WSL stop the VM. k3s comes back by itself when the VM restarts.

## Real llama.cpp replicas (CPU load you can see)

`deploy/k3s/llama` swaps both pools for llama.cpp pods. The Deployment names and labels stay the same, so nothing else needs rewiring.

| | |
|---|---|
| Image | official llama.cpp server image, build b11913, pinned by digest, about 309 MB to download |
| Model | the pinned Qwen2.5-0.5B GGUF, already on disk |
| Each pod | 2 CPUs requested and limited, `-t 2 -np 4`, 1024 tokens per slot |
| Budget | 2 replicas per pool, which keeps the laptop's 8-thread cap |

The model reaches the pods through a PersistentVolume. The restricted Pod Security Standard forbids hostPath volumes in pods.

`scripts/k3s/llama-up.sh` does the switch:
- copies the model to the node and checks its pinned SHA-256;
- pulls the image if it is absent (ask first);
- shrinks the tenants to a 1/1, b 1/1, c 0/2 (priority 50);
- gives the controller `-capacity 2` and `deploy/k3s/llama/15-config.yaml` (C = 117, 10 s cycle);
- points both gateways at that config (2 requests in flight per replica);
- sets KEDA to 1..2 replicas at 8 in flight per replica;
- rolls the pools out.

Run it as root in WSL:

```bash
sudo bash scripts/k3s/llama-up.sh "/mnt/c/.../.cache/models/qwen2.5-0.5b-instruct-q4_k_m.gguf"
```

Then start the dashboard with `-observe-replicas llama`, which `demo-up.ps1` passes by default. The badge then reads "KUBERNETES (k3s): REAL llama.cpp replicas".

These pods run unpinned in a VM, so their latencies are real but not comparable with `results/real/`. The 2 s SLO and C = 117 were measured on bare metal. Evidence: `results/k8s/2026-10-28-k3s-llama-side-by-side/`.

To go back to simulated replicas, run `up.sh` and `kubectl apply -f deploy/k3s/side-by-side/` again.

## Afterwards

```bash
kubectl delete namespace tenantscaler        # pods, Secret, tenants
kubectl delete crd tenantscalers.scaling.tenantscaler.dev
sudo systemctl stop k3s                      # or uninstall: /usr/local/bin/k3s-uninstall.sh
```

Then, in Windows:
- Run `Remove-NetFirewallHyperVRule -Name TenantScalerGW` (Administrator).
- Run `./scripts/k3s/keys.ps1 -Force` so the keys handed out stop working at the next deploy. Clipboard history may still hold them.
- Run `wsl --shutdown` to free the VM's memory.

If the laptop changes networks, the node's IP changes with it in mirrored mode, for example when the hotspot reconnects. k3s keeps the old address, and then several things break:
- pods time out on the API server (`10.43.0.1`);
- the gateways stop discovering pods;
- the controller fails static;
- KEDA crash-loops.

`sudo bash scripts/k3s/heal.sh` fixes it: it restarts k3s and then KEDA. `demo-up.ps1` runs it first.
