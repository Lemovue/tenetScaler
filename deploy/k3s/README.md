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

If the laptop changes networks, the node's IP changes with it in mirrored mode. Run `sudo systemctl restart k3s`.
