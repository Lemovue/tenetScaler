// Results tab: renders the evaluation summaries served by /api/results,
// the REAL-replica summary (tenantscaler realrun) and every SIMULATED one
// (tenantscaler experiment: results/sim/<run>/summary.json and the like),
// each under its own label. Cells are "mean ± 95% CI" over the runs,
// exactly as the summary files hold them.
"use strict";

window.TS = window.TS || {};

(function () {
  const { el, clear, num, isNum } = TS.util;

  const LABELS = {
    victim_violation_pct: "Victims' SLO violations in the spike (%)",
    victim_shed_aware_violation_pct: "Victims' violations, shed counted as misses (%)",
    noisy_shed_pct: "Noisy tenant's requests shed (%)",
    reaction_time_s: "Reaction time (s)",
    replica_minutes: "Replica-minutes",
    jain_index: "Jain's fairness index (spike)",
    max_counter_gap: "Max VTC counter gap",
    reclaim_latency_s: "Reclaim latency (s)",
    policy_latency_mean_ms: "Policy propagation, mean (ms)",
    completed: "Completed requests",
    shed: "Shed requests",
    failed: "Failed requests",
  };

  function label(name) {
    if (LABELS[name]) return LABELS[name];
    const m = /^p99_(ttft|tpot)_s_(.+)$/.exec(name);
    if (m) return "p99 " + m[1].toUpperCase() + ", tenant " + m[2] + " (s)";
    return name;
  }

  // orderMetrics lists the known metrics first, in report order, then the
  // per-tenant percentiles, then anything else.
  function orderMetrics(names) {
    const known = Object.keys(LABELS);
    const rank = (n) => {
      const i = known.indexOf(n);
      if (i >= 0 && i < 9) return [0, i];
      if (n.startsWith("p99_")) return [1, 0];
      return i >= 0 ? [2, i] : [3, 0];
    };
    return names.slice().sort((a, b) => {
      const ra = rank(a);
      const rb = rank(b);
      return ra[0] - rb[0] || ra[1] - rb[1] || a.localeCompare(b);
    });
  }

  // cell formats one eval.Stat: "mean ± ci (n=k)".
  function cell(st, runs) {
    if (!st || !isNum(st.Mean)) return "n/a";
    let s = num(st.Mean, 3);
    if (isNum(st.CI95)) s += " ± " + num(st.CI95, 3);
    if (isNum(st.N) && st.N !== runs) s += " (n=" + st.N + ")";
    return s;
  }

  // statsTable renders [{setup, runs, metrics: {name: Stat}}] with one row
  // per metric and one column per setup.
  function statsTable(stats, caption) {
    const names = new Set();
    for (const s of stats) Object.keys(s.metrics || {}).forEach((n) => names.add(n));
    const head = el("tr", {}, [el("th", { scope: "col" }, "Metric")].concat(
      stats.map((s) => el("th", { scope: "col" }, s.setup + " (N=" + s.runs + ")"))));
    const body = el("tbody");
    for (const n of orderMetrics(Array.from(names))) {
      body.appendChild(el("tr", {}, [el("th", { scope: "row" }, label(n))].concat(
        stats.map((s) => el("td", {}, cell((s.metrics || {})[n], s.runs))))));
    }
    return el("div", { class: "table-wrap" }, el("table", {}, [
      el("caption", {}, caption), el("thead", {}, head), body]));
  }

  function badge(simulated) {
    return el("span", { class: simulated ? "badge badge-sim" : "badge badge-real" }, simulated ? "SIMULATED" : "REAL");
  }

  // injectedBadge marks real runs whose replicas had the injected load delay
  // (readinessHoldSeconds > 0); null otherwise.
  function injectedBadge(s) {
    if (!isNum(s.readinessHoldSeconds) || s.readinessHoldSeconds <= 0) return null;
    return el("span", { class: "badge badge-injected", title: "Injected load delay (emulates network model load); inference is real" },
      "INJECTED " + num(s.readinessHoldSeconds, 0) + " s LOAD DELAY");
  }

  function section(title, file, emptyText, renderSummary) {
    const sec = el("section", { class: "results-section card" });
    if (!file || !file.present) {
      sec.appendChild(el("h2", {}, title));
      sec.appendChild(el("p", { class: "empty" }, emptyText));
      return sec;
    }
    if (file.error) {
      sec.appendChild(el("h2", {}, title));
      sec.appendChild(el("p", { class: "banner banner-error", role: "alert" }, "Cannot show " + file.path + ": " + file.error));
      return sec;
    }
    renderSummary(sec, file.summary || {}, file.path);
    return sec;
  }

  function renderReal(sec, s, path) {
    sec.appendChild(el("div", { class: "section-head" }, [el("h2", {}, "Real replicas"),
      el("span", { class: "badges" }, [badge(!!s.simulated), injectedBadge(s)])]));
    sec.appendChild(el("p", { class: "results-label" }, s.label || "No label in the summary."));
    const sc = s.scenario || {};
    sec.appendChild(el("p", { class: "muted" }, "Scenario " + (sc.name || "?") + " (" + num(sc.durationSeconds, 0) +
      " s), " + (s.runs || "?") + " runs per setup, seeds " + (s.seeds || []).join(", ") + "; " + path));
    if (!(s.stats || []).length) {
      sec.appendChild(el("p", { class: "empty" }, "The summary holds no completed runs."));
      return;
    }
    sec.appendChild(statsTable(s.stats, "Mean ± 95% CI over runs"));
  }

  function simTitle(name) {
    return "Simulated evaluation" + (name ? ": " + name : "") + " (virtual time)";
  }

  // renderSim renders one simulated summary; name is its run (the
  // directory under results/sim), budget and scenario come from the file.
  function renderSim(sec, s, path, name) {
    sec.appendChild(el("div", { class: "section-head" }, [el("h2", {}, simTitle(name)), badge(true)]));
    if (s.disclaimer) sec.appendChild(el("p", { class: "results-label" }, s.disclaimer));
    const results = s.results || [];
    const main = results.find((r) => r.coldStartSeconds === s.defaultColdStartSeconds) || results[0];
    const budget = (s.parameters || {}).capacityReplicas;
    sec.appendChild(el("p", { class: "muted" }, "Scenario " + (s.scenario || "?") + " (" + num(s.scenarioSeconds, 0) +
      " s)" + (isNum(budget) ? ", budget " + budget + " replicas" : "") + ", " + (s.runs || "?") + " runs per setup" +
      (main ? ", cold start " + num(main.coldStartSeconds, 0) + " s" : "") + "; " + path));
    if (!main || !(main.setups || []).length) {
      sec.appendChild(el("p", { class: "empty" }, "The summary holds no results."));
      return;
    }
    sec.appendChild(statsTable(main.setups, "SIMULATED: mean ± 95% CI over runs"));
  }

  // load fetches /api/results and renders the real summary, then every
  // simulated one, into container.
  async function load(container) {
    container.setAttribute("aria-busy", "true");
    try {
      const r = await TS.util.getJSON("/api/results");
      clear(container);
      const real = r.real || {};
      container.appendChild(section("Real replicas", real, "No real results yet (looked for " + (real.path || "the summary") +
        "): run `tenantscaler realrun -config config/real.yaml` to create them.", renderReal));
      const sims = r.simulated || [];
      if (!sims.length) {
        container.appendChild(section(simTitle(""), null, "No simulated results yet (looked for " +
          (r.simulatedSearched || []).join(", ") + "): run `tenantscaler experiment -runs 10 -out results/sim/default` to create them.", renderSim));
      }
      for (const f of sims) {
        container.appendChild(section(simTitle(f.name), f, "", (sec, s, path) => renderSim(sec, s, path, f.name)));
      }
    } catch (err) {
      clear(container);
      container.appendChild(el("p", { class: "banner banner-error", role: "alert" }, "Could not load the results: " + err.message));
    } finally {
      container.removeAttribute("aria-busy");
    }
  }

  TS.Results = { load };
})();
