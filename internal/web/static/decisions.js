// Decision log of the TenantScaler controller: newest first, filterable by
// tenant; each record expands (a native <details>, keyboard accessible) to
// the numbers it was based on and the arbiter's rule steps.
"use strict";

window.TS = window.TS || {};

(function () {
  const { el, setText, clear, num } = TS.util;
  const LIMIT = 200;

  class DecisionLog {
    // nodes: {select, list, status}
    constructor(nodes) {
      this.select = nodes.select;
      this.list = nodes.list;
      this.status = nodes.status;
      this.open = new Set(); // keys of expanded records, kept across refreshes
      this.signature = "";
      this.pending = false; // a request is in flight
      this.dirty = false; // a refresh was asked for meanwhile
      this.poolTenant = "";
      this.select.addEventListener("change", () => {
        this.signature = "";
        this.refresh();
      });
    }

    // setTenants fills the filter: every tenant, then the pool's own
    // records (poolTenant is their tenant name).
    setTenants(names, poolTenant) {
      this.poolTenant = poolTenant || "";
      for (const n of names) this.select.appendChild(el("option", { value: n }, "Tenant " + n));
      if (this.poolTenant) this.select.appendChild(el("option", { value: this.poolTenant }, "Pool (budget and stabilizer)"));
    }

    // refresh loads the records of the selected tenant. One request runs
    // at a time; a refresh asked for meanwhile (a filter change, a poll)
    // runs when it returns, and a reply for a tenant no longer selected is
    // dropped, so the list always ends up showing the selected tenant.
    async refresh() {
      if (this.pending) {
        this.dirty = true;
        return;
      }
      this.pending = true;
      this.dirty = false;
      const tenant = this.select.value;
      const url = "/api/decisions?limit=" + LIMIT + (tenant ? "&tenant=" + encodeURIComponent(tenant) : "");
      try {
        const reply = await TS.util.getJSON(url);
        if (tenant === this.select.value) this.render(reply, tenant);
      } catch (err) {
        if (tenant === this.select.value) setText(this.status, "Could not load the decision log: " + err.message);
      } finally {
        this.pending = false;
        if (this.dirty) this.refresh();
      }
    }

    render(reply, tenant) {
      const recs = reply.records || [];
      const first = recs.length ? recs[0].cycle + "/" + recs[0].tenant : "";
      const sig = [tenant, reply.total, first, recs.length, reply.live].join("|");
      if (sig === this.signature) return;
      this.signature = sig;
      const who = tenant ? "tenant " + tenant : "all tenants";
      if (!recs.length) {
        this.list.hidden = true;
        setText(this.status, reply.total
          ? "No decisions for " + who + " yet."
          : "No decisions yet: they appear while the TenantScaler stack runs.");
        return;
      }
      setText(this.status, (reply.live ? "Live: " : "Last TenantScaler run: ") + recs.length + " of " +
        reply.total + " records (" + who + "), newest first. Expand a record for its inputs and rule steps.");
      const origin = reply.trafficStartedAt ? Date.parse(reply.trafficStartedAt) : NaN;
      const focused = document.activeElement && document.activeElement.closest
        ? document.activeElement.closest("li[data-key]") : null;
      const focusKey = focused && this.list.contains(focused) ? focused.dataset.key : "";
      clear(this.list);
      for (const r of recs) this.list.appendChild(this.item(r, origin));
      this.list.hidden = false;
      if (focusKey) {
        const again = Array.from(this.list.children).find((li) => li.dataset.key === focusKey);
        if (again) again.querySelector("summary").focus();
      }
    }

    item(r, origin) {
      const key = r.cycle + "/" + r.tenant;
      const t = Date.parse(r.time);
      const when = Number.isFinite(origin) && Number.isFinite(t)
        ? "t = " + ((t - origin) / 1000).toFixed(1) + " s"
        : new Date(t).toLocaleTimeString();
      const head = el("span", { class: "dec-meta" }, when + " · cycle " + r.cycle + " · policy v" + r.policyVersion);
      const pool = r.tenant === this.poolTenant;
      const who = pool ? "Pool" : "Tenant " + r.tenant;
      const title = el("strong", {}, " " + who + ": " + r.final.grant + " replica" + (r.final.grant === 1 ? "" : "s"));
      const flags = [];
      if (r.final.shedding) flags.push(el("span", { class: "flag flag-warn" }, "sheddable requests get 429"));
      if (r.failStatic) flags.push(el("span", { class: "flag flag-warn" }, "fail-static"));
      // Comic captions made from this record (comic.js) are listed here too.
      for (const c of TS.Comic ? TS.Comic.captionsFor(key) : []) flags.push(el("span", { class: "flag flag-caption" }, "Caption: " + c));
      const summary = el("summary", {}, [head, title].concat(flags, [el("span", { class: "dec-reason" }, r.reason)]));
      const details = el("details", {}, [summary, this.body(r, pool)]);
      if (this.open.has(key)) details.open = true;
      details.addEventListener("toggle", () => {
        if (details.open) this.open.add(key);
        else this.open.delete(key);
      });
      return el("li", { "data-key": key }, details);
    }

    // body lists the inputs, rule steps and outcome of a tenant record; a
    // pool record has only its rule steps and the replicas applied.
    body(r, pool) {
      const steps = (r.steps || []).map((s) =>
        el("li", {}, s.rule + " → " + s.grant + (s.note ? ": " + s.note : "")));
      const stepList = el("div", {}, [el("h3", {}, "Rule steps"), steps.length ? el("ol", {}, steps) : el("p", { class: "empty" }, "none")]);
      if (pool) {
        return el("div", { class: "dec-body" }, [stepList, el("div", {}, [el("h3", {}, "Outcome"), dl([["Pool replicas", r.final.grant]])])]);
      }
      const inp = r.inputs;
      const rows = [
        ["Demand (replicas)", inp.demand],
        ["Reactive demand", inp.reactiveDemand],
        ["Forecast demand", inp.forecastDemand === undefined ? "not used" : inp.forecastDemand],
        ["Guaranteed", inp.guaranteed],
        ["Max replicas", inp.maxReplicas],
        ["Priority", inp.priority],
        ["SLO slack", num(inp.sloSlack, 2)],
        ["Weighted tokens/s", num(inp.weightedTokenRate, 1)],
      ];
      const outcome = [
        ["Grant", r.final.grant],
        ["Shedding", r.final.shedding ? "yes" : "no"],
        ["Gateway weight", num(r.final.weight, 2)],
      ];
      return el("div", { class: "dec-body" }, [
        el("div", {}, [el("h3", {}, "Inputs"), dl(rows)]),
        stepList,
        el("div", {}, [el("h3", {}, "Outcome"), dl(outcome)]),
      ]);
    }
  }

  function dl(rows) {
    const node = el("dl");
    for (const [k, v] of rows) {
      node.appendChild(el("dt", {}, k));
      node.appendChild(el("dd", {}, String(v)));
    }
    return node;
  }

  TS.DecisionLog = DecisionLog;
})();
