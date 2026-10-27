// TenantScaler dashboard: polls /api/state, drives Run A/B and Stop, and
// renders the two stacks side by side (live p99 TTFT and replica charts,
// per-tenant tables), the summary, the decision log and the Results tab.
"use strict";

window.TS = window.TS || {};

(function () {
  const U = TS.util;
  const $ = (id) => document.getElementById(id);
  // Tenant colours in configuration order (validated categorical slots).
  const SLOTS = ["--series-1", "--series-2", "--series-3", "--series-4", "--series-5", "--series-6", "--series-7", "--series-8"];
  const STATUS_TEXT = {
    pending: "waiting", starting: "starting replicas", running: "replaying traffic", draining: "draining",
    stopping: "stopping", done: "✓ done", stopped: "stopped", skipped: "not run", error: "error",
  };

  let config = null;
  let timer = 0;
  let starting = false;
  let decisionKey = "";
  let summaryKey = "";
  let resultsLoaded = false;
  const panels = {};
  let decisions = null;

  function tenantColor(i) {
    return SLOTS[i] || null;
  }

  function buildPanels() {
    for (const root of document.querySelectorAll("article.panel[data-key]")) {
      const q = (role) => root.querySelector('[data-role="' + role + '"]');
      panels[root.dataset.key] = {
        status: q("status"), setup: q("setup"), error: q("error"), windowNote: q("window-note"),
        rows: q("tenant-rows"), scope: q("table-scope"), engine: q("engine"),
        ttftLegend: q("ttft-legend"), replicasLegend: q("replicas-legend"),
        ttft: new TS.LineChart(q("ttft"), { title: root.querySelector("h2").textContent + " p99 TTFT", yFormat: U.sec,
          yTick: (v) => (Number.isInteger(v) ? String(v) : v.toFixed(1)) + " s" }),
        replicas: new TS.LineChart(q("replicas"), { title: root.querySelector("h2").textContent + " replicas", yFormat: (v) => String(v) }),
      };
    }
  }

  function setupTabs() {
    const tabs = Array.from(document.querySelectorAll('[role="tab"]'));
    const select = (tab) => {
      for (const t of tabs) {
        const on = t === tab;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
        $(t.getAttribute("aria-controls")).hidden = !on;
      }
      history.replaceState(null, "", tab.id === "tab-results" ? "#results" : location.pathname + location.search);
      if (tab.id === "tab-results" && !resultsLoaded) {
        resultsLoaded = true;
        TS.Results.load($("results-body"));
      }
    };
    tabs.forEach((tab, i) => {
      tab.addEventListener("click", () => select(tab));
      tab.addEventListener("keydown", (e) => {
        const moves = { ArrowLeft: i - 1, ArrowRight: i + 1, Home: 0, End: tabs.length - 1 };
        if (!(e.key in moves)) return;
        e.preventDefault();
        const next = tabs[(moves[e.key] + tabs.length) % tabs.length];
        next.focus();
        select(next);
      });
    });
    $("results-refresh").addEventListener("click", () => TS.Results.load($("results-body")));
    if (location.hash === "#results") select($("tab-results"));
  }

  function renderConfig(c) {
    const badge = $("backend-badge");
    badge.className = "badge " + (c.backend.simulated ? "badge-sim" : "badge-real");
    U.setText(badge, c.backend.label);
    U.setText($("backend-detail"), c.backend.detail + " Config: " + c.backend.configPath + ".");
    const sc = c.scenario;
    const kinds = sc.tenants.map((t) => t.name + " (" + t.kind + ")").join(", ");
    U.setText($("scenario-text"), "Scenario " + sc.name + ": " + sc.durationSeconds + " s of traffic, replayed on each stack in turn. Tenants " +
      kinds + (sc.noisy ? "; " + sc.noisy + " spikes from " + sc.spikeStartSeconds + " s to " + sc.spikeEndSeconds + " s, the victims are " +
      andList(sc.victims) : "") + ". " + sloText(sc.tenants) + " Each stack: budget " + c.capacityReplicas + " replicas, " +
      c.initialReplicas + " warm at the start, control cycle " + c.cycleSeconds + " s.");
    const tenants = sc.tenants.map((t, i) => ({ name: t.name, colorVar: tenantColor(i) })).filter((t) => t.colorVar);
    for (const s of c.stacks) {
      const p = panels[s.key];
      if (!p) continue;
      U.setText(p.setup, s.setup + " · gateway: " + s.gateway + " · scaling: " + s.controlPlane);
      U.setText(p.windowNote, "(live, last " + c.windowSeconds + " s, gateway-measured)");
      TS.renderLegend(p.ttftLegend, tenants.concat([{ name: "TTFT SLO", colorVar: "--threshold", dash: true }]));
      TS.renderLegend(p.replicasLegend, [{ name: "ready", colorVar: "--rep-ready" },
        { name: "starting (cold start)", colorVar: "--rep-starting", dash: "dot" }, { name: "budget", colorVar: "--threshold", dash: true }]);
    }
    decisions.setTenants(sc.tenants.map((t) => t.name), c.poolRecordTenant);
  }

  // andList joins names as "A", "A and B", "A, B and C".
  function andList(names) {
    const list = names || [];
    return list.length < 2 ? list.join("") : list.slice(0, -1).join(", ") + " and " + list[list.length - 1];
  }

  // groupBy groups items by key(item), in first-seen order: [{key, items}].
  function groupBy(items, key) {
    const groups = new Map();
    for (const it of items) {
      const k = key(it);
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(it);
    }
    return Array.from(groups, ([k, list]) => ({ key: k, items: list }));
  }

  // sloText states the configured SLOs: one sentence when every tenant
  // shares it, else one clause per group of tenants with the same SLO.
  function sloText(tenants) {
    if (!tenants.length) return "";
    const slo = (t) => "TTFT ≤ " + t.ttftSeconds + " s and TPOT ≤ " + t.tpotMillis + " ms for " +
      Number((t.targetFraction * 100).toFixed(1)) + "% of requests";
    const groups = groupBy(tenants, (t) => [t.ttftSeconds, t.tpotMillis, t.targetFraction].join("|"));
    if (groups.length === 1) return "SLO of every tenant: " + slo(tenants[0]) + ".";
    return "SLOs: " + groups.map((g) => andList(g.items.map((t) => t.name)) + " " + slo(g.items[0])).join("; ") + ".";
  }

  // sloThresholds are the TTFT SLO lines of the chart, each naming its
  // tenants when the tenants' SLOs differ.
  function sloThresholds(tenants) {
    const groups = groupBy(tenants, (t) => t.ttftSeconds);
    return groups.map((g) => {
      const v = g.items[0].ttftSeconds;
      const who = groups.length > 1 ? " (" + g.items.map((t) => t.name).join(", ") + ")" : "";
      return { y: v, label: "SLO " + (v < 1 ? U.sec(v) : v + " s") + who };
    });
  }

  async function loadConfig() {
    try {
      config = await U.getJSON("/api/config");
      renderConfig(config);
      poll();
    } catch (err) {
      showConnError(err);
      setTimeout(loadConfig, 4000);
    }
  }

  function showConnError(err) {
    const b = $("conn-error");
    U.setText(b, "Cannot reach the demo server (" + err.message + "). Retrying every few seconds.");
    b.hidden = false;
  }

  async function poll() {
    clearTimeout(timer);
    let running = false;
    try {
      const state = await U.getJSON("/api/state", 8000);
      $("conn-error").hidden = true;
      render(state);
      running = state.phase === "running";
    } catch (err) {
      showConnError(err);
    }
    timer = setTimeout(poll, running ? 1000 : 4000);
  }

  function render(state) {
    const pct = Math.round(Math.min(Math.max(state.progress, 0), 1) * 100);
    const running = state.phase === "running";
    $("progress-bar").style.width = pct + "%";
    $("progress").setAttribute("aria-valuenow", String(pct));
    // The live region announces phase changes only; the per-second detail
    // sits next to it, outside the live region.
    const phase = phaseSummary(state);
    if ($("phase-text").textContent !== phase) U.setText($("phase-text"), phase);
    U.setText($("phase-detail"), running ? pct + "% · " + state.message : state.phase === "stopped" && !state.error ? state.message : "");
    $("run-btn").disabled = running || starting;
    $("stop-btn").disabled = !running;
    // The server's run error: a failed run, or a stop whose shutdown
    // reported an error. Errors of the user's own start and stop requests
    // live in #action-error, which only those actions change.
    const err = $("run-error");
    err.hidden = !state.error;
    if (state.error) {
      U.setText(err, state.phase === "error" ? "The A/B run failed: " + state.error : state.message + ". Cause: " + state.error);
    }

    const shared = sharedScales(state);
    renderLive(state);
    for (const s of state.stacks) if (panels[s.key]) renderPanel(panels[s.key], s, state, shared);
    const sk = state.run + ":" + state.phase + ":" + state.stacks.map((s) => s.status).join(",");
    if (sk !== summaryKey) {
      summaryKey = sk;
      TS.Summary.render($("summary-body"), state);
    }

    const ts = state.stacks[1] || {};
    const live = ["running", "draining", "stopping"].includes(ts.status);
    const key = state.run + ":" + ts.status;
    if (live || key !== decisionKey) {
      decisionKey = key;
      decisions.refresh();
    }
  }

  // renderLive shows the live-demo banner: traffic from teammates' laptops
  // is not the experiment, and its numbers are not results (decision Q16).
  function renderLive(state) {
    const lb = $("live-banner");
    lb.hidden = !state.live;
    if (!state.live) return;
    const where = state.liveAddress ? " Teammates send traffic to http://" + state.liveAddress + "." : "";
    U.setText(lb, "LIVE DEMO \u2014 live demo, not the experiment: the TenantScaler stack serves traffic from teammates' " +
      "laptops, so these numbers are not experiment results." + where);
  }

  // phaseSummary is the coarse phase announced to screen readers.
  function phaseSummary(state) {
    switch (state.phase) {
      case "idle": return "Ready to run the A/B comparison.";
      case "done": return "Done: both stacks finished.";
      case "stopped": return state.error ? "Stopped, but shutting the stack down reported an error." : "Stopped.";
      case "error": return "The A/B run failed.";
    }
    const active = state.stacks.find((s) => ["starting", "running", "draining", "stopping"].includes(s.status));
    if (!active) return "Between stacks.";
    const title = active.key === "baseline" ? "Baseline" : "TenantScaler";
    return title + ": " + (STATUS_TEXT[active.status] || active.status) + ".";
  }

  // sharedScales gives both panels the same axes, so they compare at a
  // glance: x up to the longest run, y up to the largest p99 seen (at
  // least 1.5 x the SLO).
  function sharedScales(state) {
    let xMax = state.scenario.durationSeconds;
    let yMax = 0;
    for (const t of state.scenario.tenants) yMax = Math.max(yMax, 1.5 * t.ttftSeconds);
    for (const s of state.stacks) {
      for (const smp of s.samples) {
        xMax = Math.max(xMax, smp.t);
        for (const tw of Object.values(smp.tenants || {})) {
          if (U.isNum(tw.p99TtftSeconds)) yMax = Math.max(yMax, 1.1 * tw.p99TtftSeconds);
        }
      }
    }
    return { xMax, yMax };
  }

  function emptyText(s) {
    if (s.status === "pending") return "Not run yet";
    if (s.status === "starting") return "Starting warm replicas…";
    if (s.status === "skipped") return "Not run";
    if (s.status === "stopped" || s.status === "error") return "No samples";
    return "Waiting for the first sample…";
  }

  function renderPanel(p, s, state, shared) {
    const sc = state.scenario;
    let status = STATUS_TEXT[s.status] || s.status;
    if (s.status === "running") status += " · " + Math.round(s.elapsedSeconds) + " / " + sc.durationSeconds + " s";
    U.setText(p.status, status);
    p.status.className = "status" + (s.status === "done" ? " status-done" : s.status === "error" ? " status-error" : "");
    p.error.hidden = !s.error;
    U.setText(p.error, s.error ? "Error: " + s.error : "");

    const xs = s.samples.map((x) => x.t);
    p.ttft.update({
      xs, xMax: shared.xMax, yMax: shared.yMax, empty: emptyText(s),
      series: sc.tenants.map((t, i) => ({ name: t.name, colorVar: tenantColor(i),
        ys: s.samples.map((x) => (x.tenants && x.tenants[t.name] ? x.tenants[t.name].p99TtftSeconds : null)) }))
        .filter((x) => x.colorVar),
      thresholds: sloThresholds(sc.tenants),
      band: sc.noisy ? { x0: sc.spikeStartSeconds, x1: sc.spikeEndSeconds, label: sc.noisy + " spike" } : null,
    });
    const cap = config ? config.capacityReplicas : 1;
    p.replicas.update({
      xs, xMax: shared.xMax, yMax: cap * 1.15, empty: emptyText(s),
      series: [
        { name: "ready", colorVar: "--rep-ready", step: true, ys: s.samples.map((x) => x.ready) },
        { name: "starting", colorVar: "--rep-starting", step: true, dash: "dot", ys: s.samples.map((x) => x.starting) },
      ],
      thresholds: [{ y: cap, label: "budget " + cap }],
    });
    renderTenantTable(p, s, sc, state.windowSeconds);
    renderEngine(p, s, state);
  }

  function tenantName(t, sc) {
    if (t === sc.noisy) return t + " (spiking)";
    if ((sc.victims || []).includes(t)) return t + " (victim)";
    return t;
  }

  function renderTenantTable(p, s, sc, windowSeconds) {
    U.clear(p.rows);
    const row = (cells) => p.rows.appendChild(U.el("tr", {}, cells.map((c, i) => U.el(i ? "td" : "th", i ? {} : { scope: "row" }, c))));
    if (s.metrics && s.tenants) {
      U.setText(p.scope, "(whole run, client-measured)");
      for (const t of s.tenants) {
        row([tenantName(t.tenant, sc), U.pct(t.attainmentPct), U.sec(t.p99TtftSeconds), t.shed + " (" + U.pct(t.shedPct) + ")"]);
      }
      return;
    }
    const last = s.samples.length ? s.samples[s.samples.length - 1] : null;
    if (last && last.tenants) {
      U.setText(p.scope, "(live, last " + windowSeconds + " s, gateway-measured)");
      for (const t of sc.tenants) {
        const tw = last.tenants[t.name] || {};
        row([tenantName(t.name, sc), U.isNum(tw.attainment) ? U.pct(tw.attainment * 100) : "no completions",
          U.sec(tw.p99TtftSeconds), U.isNum(tw.shed) ? tw.shed + " in " + windowSeconds + " s" : "—"]);
      }
      return;
    }
    U.setText(p.scope, "(no data yet)");
    for (const t of sc.tenants) row([tenantName(t.name, sc), "—", "—", "—"]);
  }

  function renderEngine(p, s, state) {
    const parts = [];
    if (s.engine) {
      parts.push(s.engine.simulated || s.simulated ? "Engine: SIMULATED (modelled latencies, not real inference)"
        : "Engine: llama.cpp " + (s.engine.build || "unknown build") + ", model " + modelFile(s.engine.modelPath) +
          (state.backend.cpu ? ", " + state.backend.cpu : ""));
    } else if (state.backend) {
      parts.push(state.backend.simulated ? "Engine: SIMULATED" : "Engine: REAL llama.cpp");
    }
    if (s.requests) parts.push(s.requests + " requests in the trace");
    if (s.drainTimedOut) parts.push("some requests were still running after the drain timeout and count as failed");
    U.setText(p.engine, parts.join(" · "));
  }

  function modelFile(path) {
    if (!path) return "unknown model";
    return path.split(/[\\/]/).pop();
  }

  // showActionError shows why the user's start or stop request failed
  // ("" hides it). render() never touches it, so it stays until the next
  // start or stop.
  function showActionError(text) {
    const b = $("action-error");
    U.setText(b, text);
    b.hidden = !text;
  }

  async function startRun() {
    starting = true;
    $("run-btn").disabled = true;
    showActionError("");
    try {
      const r = await U.requestJSON("POST", "/api/ab/start", {});
      if (r.status !== 202) showActionError("Could not start: " + ((r.body && r.body.error) || "HTTP " + r.status));
    } catch (err) {
      showActionError("Could not start: " + err.message);
    } finally {
      starting = false;
      poll();
    }
  }

  async function stopRun() {
    $("stop-btn").disabled = true;
    showActionError("");
    U.setText($("phase-text"), "Stopping: shutting the stack down…");
    try {
      const r = await U.requestJSON("POST", "/api/ab/stop", undefined, 70000);
      if (r.status !== 200 && r.status !== 202 && r.status !== 409) {
        showActionError("Could not stop: " + ((r.body && r.body.error) || "HTTP " + r.status));
      }
    } catch (err) {
      showActionError("Could not stop: " + err.message);
    } finally {
      poll();
    }
  }

  function init() {
    buildPanels();
    decisions = new TS.DecisionLog({ select: $("dec-tenant"), list: $("dec-list"), status: $("dec-status") });
    setupTabs();
    $("run-btn").addEventListener("click", startRun);
    $("stop-btn").addEventListener("click", stopRun);
    loadConfig();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
