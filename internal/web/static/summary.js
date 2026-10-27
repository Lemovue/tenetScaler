// Side-by-side summary of a finished A/B run: the victims' SLO violations
// during the spike, replica-minutes and the other run metrics of both
// stacks, with the change from the baseline to TenantScaler. Every number
// comes from the stacks' measured eval.RunMetrics.
"use strict";

window.TS = window.TS || {};

(function () {
  const { el, clear, num, isNum } = TS.util;

  // rows: [label, read(metrics), unit]; unit "pct" shows the change in
  // percentage points.
  function rows(tenants) {
    const out = [
      ["Victims' SLO violations in the spike window", (m) => m.VictimViolationPct, "pct"],
      ["… counting shed requests as misses", (m) => m.VictimShedAwareViolationPct, "pct"],
      ["Noisy tenant's requests shed", (m) => m.NoisyShedPct, "pct"],
      ["Replica-minutes", (m) => m.ReplicaMinutes, "num"],
      ["Reaction time to the spike (s)", (m) => m.ReactionTimeSec, "num"],
      ["Jain's fairness index (spike window)", (m) => m.JainIndex, "num"],
    ];
    for (const t of tenants) out.push(["p99 TTFT, tenant " + t + " (s)", (m) => (m.P99TTFT || {})[t], "num"]);
    out.push(["Completed requests", (m) => m.Completed, "int"]);
    out.push(["Shed requests", (m) => m.Shed, "int"]);
    out.push(["Failed requests", (m) => m.Failed, "int"]);
    return out;
  }

  function fmt(v, unit) {
    if (!isNum(v)) return "n/a";
    if (unit === "pct") return num(v, 1) + "%";
    if (unit === "int") return String(v);
    return num(v, 2);
  }

  function change(a, b, unit) {
    if (!isNum(a) || !isNum(b)) return "n/a";
    const digits = unit === "int" ? 0 : unit === "pct" ? 1 : 2;
    const d = Number((b - a).toFixed(digits)); // the sign of what is shown
    const sign = d > 0 ? "+" : d < 0 ? "−" : "±";
    const mag = Math.abs(d);
    if (unit === "pct") return sign + num(mag, 1) + " pp";
    if (unit === "int") return sign + String(mag);
    return sign + num(mag, 2);
  }

  // render fills container for state; it shows an empty state until both
  // stacks have metrics.
  function render(container, state) {
    clear(container);
    const [base, ts] = state.stacks;
    if (!base || !ts || !base.metrics || !ts.metrics) {
      const why = state.phase === "running" ? "Shown when both stacks have finished."
        : state.phase === "idle" ? "Run A/B to compare the two stacks."
          : "This run did not finish both stacks, so there is nothing to compare.";
      container.appendChild(el("p", { class: "empty" }, why));
      return;
    }
    const sc = state.scenario;
    const victims = (sc.victims || []).join(" and ") || "the victims";
    const real = !state.backend.simulated && !base.simulated && !ts.simulated;
    const bm = base.metrics;
    const tm = ts.metrics;
    container.appendChild(el("p", { class: "lead" },
      "Same traffic (" + base.requests + " requests, seed " + state.seed + ") on each stack. " +
      (sc.noisy ? "During " + sc.noisy + "'s spike, " : "During the run, ") + victims + " missed their SLO on " + fmt(bm.VictimViolationPct, "pct") + " of their requests with the baseline and " +
      fmt(tm.VictimViolationPct, "pct") + " with TenantScaler; the stacks used " + num(bm.ReplicaMinutes, 2) + " and " +
      num(tm.ReplicaMinutes, 2) + " replica-minutes."));
    container.appendChild(el("p", { class: "muted" }, (real
      ? "Measured on REAL llama.cpp replicas (" + state.backend.label.replace(/^REAL: /, "") + "). "
      : "SIMULATED engine: modelled latencies, not real inference. ") +
      "One run per stack is a single sample; the Results tab holds the mean ± 95% CI over repeated runs."));
    const head = el("tr", {}, ["Metric", "Baseline (" + base.setup + ")", "TenantScaler (" + ts.setup + ")", "Change"]
      .map((h) => el("th", { scope: "col" }, h)));
    const body = el("tbody");
    for (const [label, read, unit] of rows((sc.tenants || []).map((t) => t.name))) {
      const a = read(bm);
      const b = read(tm);
      body.appendChild(el("tr", {}, [el("th", { scope: "row" }, label), el("td", {}, fmt(a, unit)),
        el("td", {}, fmt(b, unit)), el("td", {}, change(a, b, unit))]));
    }
    container.appendChild(el("div", { class: "table-wrap" }, el("table", {}, [
      el("caption", { class: "visually-hidden" }, "Run metrics of both stacks"), el("thead", {}, head), body])));
  }

  TS.Summary = { render };
})();
