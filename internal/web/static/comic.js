// Comic-print layer: caption boxes for the TenantScaler controller's
// decisions, and the red/cyan split glitch of view and theme switches.
//
// Captions come only from the decision log (/api/decisions, the endpoint
// the log already uses): each is a change between a tenant's record and its
// previous one, worded from the records' own fields, and a number appears
// only when the record holds it. One caption at a time, 4 s each; each one
// is also marked on its record in the decision log panel. Captions are
// visual duplicates of the log (aria-hidden) and are off in Calm. The glitch
// animates transform and opacity only, 250 ms, and is skipped under
// prefers-reduced-motion or Calm.
"use strict";

window.TS = window.TS || {};

TS.Comic = (function () {
  const root = document.documentElement;
  const $ = (id) => document.getElementById(id);
  const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
  const SHOW_MS = 4000;
  const GAP_MS = 300;
  const GLITCH_MS = 250;
  const SWAP_MS = 90; // when, inside the glitch, the change happens
  const QUEUE_MAX = 6;

  const prev = new Map(); // tenant -> its previous record
  const seen = new Set(); // record keys already read
  const byKey = new Map(); // record key -> its captions
  const queue = [];
  let run = ""; // trafficStartedAt of the run being read
  let noisy = "";
  let pool = "";
  let showing = false;
  let busy = false;

  function calm() {
    return TS.Decor && TS.Decor.isCalm();
  }

  const replicas = (n) => n + " replica" + (n === 1 ? "" : "s");

  // captionsOf words the changes from p (the tenant's previous record) to
  // r. Numbers are the records' own integers: demand, guarantee, grant.
  function captionsOf(r, p) {
    const out = [];
    const t = r.tenant;
    const d = r.inputs || {};
    const pd = p ? p.inputs || {} : null;
    if (r.failStatic && !(p && p.failStatic)) out.push("Controller fail-static: the last policy stays in force");
    if (pd && d.demand > d.guaranteed && !(pd.demand > pd.guaranteed)) {
      out.push(t === noisy ? "Tenant " + t + " spike detected: it wants " + replicas(d.demand)
        : "Tenant " + t + " wants " + replicas(d.demand) + ", above its guarantee of " + d.guaranteed);
    }
    if (p && r.final.shedding && !p.final.shedding) out.push("Shedding " + t + "'s sheddable traffic");
    if (p && !r.final.shedding && p.final.shedding) out.push(t + "'s sheddable traffic is admitted again");
    // Reclaim: idle last cycle (its guarantee free for others), now it has
    // demand again and got its share back.
    const share = Math.min(d.demand, d.guaranteed);
    if (pd && pd.demand === 0 && share > 0 && r.final.grant >= share) {
      out.push(t + " reclaimed its share: " + replicas(r.final.grant));
    }
    const borrowed = (r.steps || []).some((s) => s.rule === "borrow") && r.final.grant > d.guaranteed;
    if (p && borrowed && !(p.final.grant > (pd ? pd.guaranteed : 0))) {
      out.push(t + " borrowed idle replicas: " + replicas(r.final.grant) + " in all");
    }
    return out;
  }

  // read takes a decisions reply (newest first) and queues the captions of
  // records not read before, oldest first.
  function read(reply) {
    if (reply.trafficStartedAt && reply.trafficStartedAt !== run) {
      run = reply.trafficStartedAt;
      prev.clear();
      seen.clear();
      byKey.clear();
      queue.length = 0;
    }
    const recs = (reply.records || []).slice().reverse();
    for (const r of recs) {
      const key = r.cycle + "/" + r.tenant;
      if (seen.has(key) || r.tenant === pool || !r.final) continue;
      seen.add(key);
      const texts = captionsOf(r, prev.get(r.tenant));
      prev.set(r.tenant, r);
      if (!texts.length) continue;
      byKey.set(key, texts);
      markLog(key, texts);
      for (const text of texts) queue.push(text);
    }
    while (queue.length > QUEUE_MAX) queue.shift();
    next();
  }

  // markLog adds the captions to their record in the decision log panel.
  function markLog(key, texts) {
    const li = document.querySelector('#dec-list li[data-key="' + key + '"] summary');
    if (!li || li.querySelector(".flag-caption")) return;
    for (const t of texts) li.appendChild(TS.util.el("span", { class: "flag flag-caption" }, "Caption: " + t));
  }

  function next() {
    const box = $("caption-box");
    if (!box || showing) return;
    if (calm() || !queue.length) {
      if (calm()) queue.length = 0;
      box.hidden = true;
      return;
    }
    showing = true;
    TS.util.setText(box.querySelector(".caption-text"), queue.shift());
    box.hidden = false;
    box.classList.remove("in");
    void box.offsetWidth; // restart the entrance
    box.classList.add("in");
    setTimeout(() => {
      box.hidden = true;
      showing = false;
      setTimeout(next, GAP_MS);
    }, SHOW_MS);
  }

  // update fetches the latest decisions while the TenantScaler stack runs
  // (app.js calls it with every decision-log refresh).
  async function update() {
    if (busy || calm()) return;
    busy = true;
    try {
      read(await TS.util.getJSON("/api/decisions?limit=60"));
    } catch (e) {
      // the decision log panel reports load errors
    } finally {
      busy = false;
    }
  }

  // glitch runs change() inside a 250 ms red/cyan split glitch, or at once
  // under reduced motion or Calm.
  function glitch(change) {
    const g = $("glitch");
    if (!g || reduce.matches || calm()) {
      change();
      return;
    }
    g.classList.remove("on");
    root.classList.remove("glitching");
    void g.offsetWidth; // restart the animation
    g.classList.add("on");
    root.classList.add("glitching");
    setTimeout(change, SWAP_MS);
    setTimeout(() => {
      g.classList.remove("on");
      root.classList.remove("glitching");
    }, GLITCH_MS);
  }

  document.addEventListener("ts:calm", () => {
    if (calm()) next();
  });

  return {
    update,
    glitch,
    configure(cfg) {
      noisy = (cfg && cfg.scenario && cfg.scenario.noisy) || "";
      pool = (cfg && cfg.poolRecordTenant) || "";
    },
    captionsFor: (key) => byKey.get(key) || [],
    // For checks: feed a decisions reply as if fetched.
    read,
  };
})();
