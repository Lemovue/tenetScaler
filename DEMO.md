# TenantScaler: 3-minute demo

Two ways to show it. **Plan A** needs no network: the dashboard replays the same recorded traffic on the KEDA baseline and then on TenantScaler, on real llama.cpp replicas. **Plan B** adds a live part: three teammates' laptops act as tenants A, B and C. Always keep **Plan C**, a recorded video of a good Plan A run.

Every number shown live comes from one run. It is a single sample, not a result. Quote results only from `results/real/summary.md` (mean ± 95% CI over runs).

## 30 minutes before

1. Plug the laptop in. Set Windows power mode to **Best performance** (Settings → System → Power).
2. Close other heavy apps. Check the machine with `python scripts/dev/resource_guard.py`; it must end with `RESULT: OK`.
3. Use a **phone hotspot, not the venue Wi-Fi**: venue networks often block laptop-to-laptop traffic. Connect every laptop to it and set the network to **Private** on Windows.
4. Build and check the demo:

   ```powershell
   ./scripts/build.ps1
   bin/tenantscaler.exe demo           # then open http://127.0.0.1:8080
   ```

   The header badge must say **REAL: llama.cpp + qwen2.5-0.5b-instruct-q4_k_m on 4 CPU-pinned replicas**. If it says **SIMULATED engine**, `.env` lacks the llama paths: run `./scripts/setup-llama.ps1`.
5. A real A/B run takes about **5–6 minutes** (2 minutes of traffic per stack, plus start-up and drain). Press **Run A/B** about 4 minutes before your slot. The baseline then finishes while you introduce the problem, and TenantScaler runs while you explain it.

## Plan A script: dashboard only (3 minutes)

| Time | Show | Say |
|---|---|---|
| 0:00 | Header badge | "Everything you see is real inference: llama.cpp and Qwen 0.5B, four replicas, each pinned to its own CPU core." |
| 0:15 | Scenario line under the buttons | "Three teams share one model. A is steady, B is periodic, C spikes in the middle. Same traffic, same budget, for both stacks." |
| 0:35 | Left panel: **Baseline: queue-based KEDA** | "KEDA sees the queue grow and scales to all four replicas, but it cannot choose *who* gets them. A's and B's time-to-first-token climb far above the dashed 2-second SLO line, though their own traffic never changed." |
| 1:10 | Right panel: **TenantScaler** | "Same traffic. TenantScaler gives every team its guaranteed share and lends idle capacity to C. When C asks for more than exists, its sheddable requests get 429 with Retry-After, and A and B stay near their SLO." |
| 1:45 | **TenantScaler decision log**: expand one record for C | "Every 10 seconds the controller explains itself: 'C got 2 replicas: guarantee 1 plus 1 borrowed; it wanted more, so its sheddable requests get 429.'" |
| 2:15 | **Side by side** summary | "This is one run, so one sample. The repeated, interleaved runs are on the Results tab." |
| 2:35 | **Results** tab | "Five setups, at least five runs each, mean with 95% confidence intervals, including a Flow-Control-style baseline. Simulated numbers are labelled." |
| 2:55 | – | "TenantScaler decides who gets the scarce replicas, and the gateway enforces it on every request." |

## Plan B: three laptops as tenants (live, about 1 minute inside the 3)

On the presenting laptop:

1. Put three tenant keys in `.env`, one per teammate, each at least 16 random characters:

   ```
   TS_TENANT_KEYS=A:standard=<key A>,B:standard=<key B>,C:sheddable=<key C>
   ```

   Give each teammate **only their own key**, and never commit `.env`.
2. Allow the port through the firewall. **Ask the laptop owner first**; this needs an Administrator PowerShell:

   ```powershell
   ./scripts/demo/firewall.ps1 -Port 8081
   ```

3. Find the hotspot IP (`ipconfig`, the Wi-Fi adapter's IPv4 address). Start the live stack:

   ```powershell
   bin/tenantscaler.exe demo -live 0.0.0.0:8081 -config config/live-demo.yaml
   ```

   `config/live-demo.yaml` is `config/real.yaml` with C at priority 50 (A and B borrow a spare replica first) and at most 2 requests in flight per replica. The dashboard shows no banner: say out loud that these are live-demo numbers, not the experiment results.

On each teammate's laptop, copy the matching binary from `dist/` (build it with `./scripts/build-dist.ps1`):

| Teammate | Command |
|---|---|
| A (steady) | `tenantscaler loadgen -gateway http://<IP>:8081 -key <key A> -profile steady` |
| B (periodic) | `tenantscaler loadgen -gateway http://<IP>:8081 -key <key B> -profile periodic` |
| C (noisy) | `tenantscaler loadgen -gateway http://<IP>:8081 -key <key C> -profile spike -spike-now` |

On Mac or Linux, use `./tenantscaler-darwin-arm64` or `./tenantscaler-linux-amd64` (`chmod +x` first). Each teammate's terminal prints a line every 10 seconds: requests answered, rejected with 429, failed, and p95 time-to-first-token. Say: "C's laptop gets 429s; A and B keep getting answers within seconds."

The key decides the tenant and its highest priority. C's key is capped at *sheddable*, so C cannot jump the queue by asking for *critical*.

## After the demo

1. Press **Stop** on the dashboard, or Ctrl+C in its terminal. This kills every llama-server it started.
2. Remove the firewall rule: `./scripts/demo/firewall.ps1 -Port 8081 -Remove`.
3. If a crash left replicas behind: `bin/tenantscaler.exe cleanup`.

## Plan C: backup video

1. Do a full Plan A run the day before and record it with **Win + Alt + R** (Xbox Game Bar), or OBS.
2. Keep the file on the laptop **and** on a USB stick; don't rely on streaming it.
3. If anything fails live, say "here is the same run recorded yesterday" and narrate the Plan A script over the video.
