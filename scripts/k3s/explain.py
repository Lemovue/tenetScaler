#!/usr/bin/env python3
"""Narrate, in plain words, what the TenantScaler cluster is doing.

For a terminal next to the dashboard (deploy/k3s/README.md). Run it in WSL
as the user who has the kubeconfig, until Ctrl+C:

    python3 scripts/k3s/explain.py --keys /mnt/c/Users/<you>/.tenantscaler/k8s.env

Every few seconds it prints only what changed:
  CONTROLLER  each tenant's grant and the controller's own reason, and the
              pool size it applies
  KEDA        the side-by-side baseline's metric and replica target
  PODS        llama pods starting, ready and stopping, in both pools
  TRAFFIC     per tenant, requests/s, 429s and p99 time to first token at
              TenantScaler's gateway and at the baseline's (needs the admin
              key from --keys; the key is never printed)
"""
import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

NS = "tenantscaler"
POOLS = {"model-pool": "TenantScaler", "model-pool-baseline": "baseline (KEDA)"}
COLOR = {"CONTROLLER": "36", "POOL": "36", "KEDA": "35", "PODS": "33", "TRAFFIC": "32", "TENANTS": "1", "ERROR": "31"}


def say(tag, text):
    stamp = time.strftime("%H:%M:%S")
    code = COLOR.get(tag, "0")
    print(f"{stamp}  \033[{code}m{tag:<10}\033[0m {text}", flush=True)


def kubectl_json(*args):
    out = subprocess.run(["kubectl", "-n", NS, *args, "-o", "json"], capture_output=True, text=True, timeout=15)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip() or "kubectl failed")
    return json.loads(out.stdout)


def http_json(url, token=None):
    req = urllib.request.Request(url)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=5) as resp:
        return json.load(resp)


def cluster_ip(service):
    return kubectl_json("get", "svc", service)["spec"]["clusterIP"]


def admin_key(path):
    if not path:
        return None
    with open(path, encoding="utf-8-sig") as f:
        for line in f:
            k, _, v = line.strip().partition("=")
            if k == "TS_ADMIN_KEY":
                return v
    return None


def plain(reason):
    """The controller's reason without the policy version, which changes every cycle."""
    return re.sub(r"Gateway policy v\d+:", "Gateway:", reason or "")


def quantity(q):
    """A Kubernetes quantity such as "4500m" or "12" as a float."""
    q = str(q)
    for suffix, scale in (("m", 1e-3), ("k", 1e3)):
        if q.endswith(suffix):
            return float(q[:-1]) * scale
    return float(q)


class Narrator:
    def __init__(self, token):
        self.token = token
        self.reasons = {}      # tenant -> last printed reason
        self.pool_reason = None
        self.keda = None
        self.pods = {}         # pod name -> last printed state
        self.urls = {}

    def url(self, service, port):
        if service not in self.urls:
            self.urls[service] = f"http://{cluster_ip(service)}:{port}"
        return self.urls[service]

    def tenants_intro(self):
        for t in sorted(kubectl_json("get", "tenantscalers")["items"], key=lambda t: t["metadata"]["name"]):
            s = t["spec"]
            role = "noisy neighbour (lower priority)" if s.get("priority", 100) < 100 else "normal team"
            say("TENANTS", f"{t['metadata']['name']}: guaranteed {s['guaranteedReplicas']} replica(s), max "
                f"{s['maxReplicas']}, priority {s.get('priority')}, SLO first token <= {s['slo']['ttftSeconds']} s"
                f"  -> {role}")

    def controller(self):
        for t in kubectl_json("get", "tenantscalers")["items"]:
            name, st = t["metadata"]["name"], t.get("status", {})
            reason = plain(st.get("reason"))
            if reason and reason != self.reasons.get(name):
                self.reasons[name] = reason
                shed = "  [SHEDDING: extra requests get 429]" if st.get("shedding") else ""
                say("CONTROLLER", f"{reason}{shed}")
        try:
            recs = http_json(self.url("tenantscaler-controller", 8090) + "/decisions?limit=8")
        except Exception:
            return
        pool = next((r for r in recs if r.get("tenant") == "(pool)"), None)
        if pool and pool.get("reason") != self.pool_reason:
            self.pool_reason = pool.get("reason")
            say("POOL", "TenantScaler " + self.pool_reason)

    def keda_view(self, heartbeat):
        """Prints when KEDA's replica count or target changes, and with
        heartbeat (about every 10 s) when its metric moved."""
        try:
            hpa = kubectl_json("get", "hpa", "keda-hpa-model-pool-baseline")
        except Exception:
            return
        st = hpa.get("status", {})
        metric = None
        for m in st.get("currentMetrics") or []:
            cur = (m.get("external") or {}).get("current") or {}
            if "averageValue" in cur:
                metric = quantity(cur["averageValue"])
        target = None
        for m in hpa["spec"].get("metrics") or []:
            t = (m.get("external") or {}).get("target") or {}
            if "averageValue" in t:
                target = quantity(t["averageValue"])
        view = (st.get("currentReplicas"), st.get("desiredReplicas"),
                None if metric is None else round(metric, 1), target)
        if view == self.keda or (not heartbeat and self.keda and view[:2] == self.keda[:2]):
            return
        self.keda = view
        cur, want, m, tgt = view
        if m is None:
            say("KEDA", f"baseline has {cur} replica(s), wants {want}; metric not readable right now")
        else:
            say("KEDA", f"baseline: {m:g} requests in flight per replica (target {tgt:g}) -> wants {want} "
                f"replica(s), has {cur}. KEDA sees only totals, not tenants.")

    def pods_view(self):
        items = kubectl_json("get", "pods", "-l", "app.kubernetes.io/name in (model-pool,model-pool-baseline)")["items"]
        seen = set()
        for p in items:
            name = p["metadata"]["name"]
            pool = POOLS.get(p["metadata"]["labels"].get("app.kubernetes.io/name"), "?")
            seen.add(name)
            ready = any(c["type"] == "Ready" and c["status"] == "True" for c in p.get("status", {}).get("conditions", []))
            if p["metadata"].get("deletionTimestamp"):
                state = "stopping (finishing its requests)"
            elif ready:
                state = "ready: llama.cpp model loaded, serving"
            else:
                state = "starting: loading the model"
            if self.pods.get(name) != state:
                self.pods[name] = state
                say("PODS", f"{pool} pod {name}: {state}")
        for name in [n for n in self.pods if n not in seen]:
            say("PODS", f"pod {name}: gone")
            del self.pods[name]

    def traffic(self):
        if not self.token:
            return
        rows = []
        for service, label in (("gateway", "TenantScaler"), ("gateway-baseline", "baseline")):
            try:
                st = http_json(self.url(service, 8080) + "/stats?window=10s", self.token)
            except Exception:
                continue
            parts = []
            for t in st.get("tenants", []):
                if t["arrivalRequestsPerSec"] == 0 and t["completed"] == 0 and t["shed"] == 0:
                    continue
                ttft = f"{t['p99TtftSeconds']:.1f}s" if t.get("hasSloData") else "-"
                parts.append(f"{t['tenant']}: {t['arrivalRequestsPerSec']:.1f} req/s, {t['shed']} x 429, p99 TTFT {ttft}")
            if parts:
                rows.append(f"{label:<12} " + " | ".join(parts))
        for r in rows:
            say("TRAFFIC", r)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--keys", help="env file with TS_ADMIN_KEY (scripts/k3s/keys.ps1); enables TRAFFIC lines")
    ap.add_argument("--every", type=float, default=2.0, help="seconds between checks (default 2)")
    args = ap.parse_args()
    if not os.environ.get("KUBECONFIG") and os.access(os.path.expanduser("~/.kube/config"), os.R_OK):
        os.environ["KUBECONFIG"] = os.path.expanduser("~/.kube/config")
    n = Narrator(admin_key(args.keys))
    say("TENANTS", "who is who (from the TenantScaler resources):")
    n.tenants_intro()
    tick = 0
    while True:
        try:
            n.pods_view()
            n.keda_view(heartbeat=tick % 5 == 0)
            if tick % 5 == 0:  # about every 10 s, one control cycle
                n.controller()
                n.traffic()
        except Exception as e:  # keep narrating through a restart
            say("ERROR", str(e)[:160])
        tick += 1
        time.sleep(args.every)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(0)
