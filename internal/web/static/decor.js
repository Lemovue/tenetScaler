// Decoration of the dashboard: the spider webs behind the page, the theme
// toggle's behaviour and the Calm switch. None of it carries information:
// the webs and the spider are aria-hidden drawings.
//
// Motion budget. On first load the corner webs are spun strand by strand
// (spokes, then the spiral from the hub outward, 1.5 s) and then stay
// still. While an A/B run is active, the webs on each panel's side tremble
// with an amplitude proportional to that stack's current request rate (the
// gateway's arrival rate in /api/state, which app.js passes in). Frames are
// drawn only while one of these animates, at most 30 a second, and never
// while the tab is hidden, under prefers-reduced-motion or in Calm. Calm is
// on automatically during runs on real replicas (decoration would share the
// CPU with the replicas being measured); it can still be switched off.
"use strict";

window.TS = window.TS || {};

TS.Decor = (function () {
  const root = document.documentElement;
  const $ = (id) => document.getElementById(id);
  const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
  const CALM_KEY = "tenantscaler-calm";
  const FRAME_MS = 1000 / 30; // frame cap
  const SPIN_MS = 1500; // first-load spinning of the corner webs
  const AMP_PX_PER_RPS = 2; // tremble: px of amplitude per request/s
  const AMP_MAX_PX = 10;
  const TREMBLE_HZ = 6;
  // SVG toggle timing (no WebGL); the drop and climb match .spider-toggle in
  // style.css.
  const DROP_MS = 160;
  const FADE_MS = 300;
  const CLIMB_MS = 260;
  // The webs: corner (t/b + l/r), size relative to the largest, seed of its
  // jitter, and the stack whose request rate shakes it (the panel on that
  // side: baseline left, TenantScaler right).
  const WEBS = [
    { corner: "tr", scale: 1, seed: 11, stack: "tenantscaler" },
    { corner: "tl", scale: 0.8, seed: 29, stack: "baseline" },
    { corner: "bl", scale: 0.65, seed: 47, stack: "baseline" },
  ];

  const webs = []; // [{canvas, ctx, spec, geo, amp, target}]
  let els = null;
  let userCalm = false;
  let run = { active: false, real: false };
  let override = false; // Calm switched off during this run
  let lastCalm = null;
  let wasStill = null;
  let spinStart = 0;
  let timer = 0;
  let raf = 0;
  let resizeTimer = 0;
  let busy = false;
  let frames = 0;

  // rng is a seeded generator (mulberry32): a web keeps its shape on every
  // redraw.
  function rng(seed) {
    let a = seed >>> 0;
    return () => {
      a = (a + 0x6d2b79f5) >>> 0;
      let t = a;
      t = Math.imul(t ^ (t >>> 15), t | 1);
      t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  // geometry lays out one web of radius R in a frame whose corner is (0, 0)
  // and whose walls are the two axes: the hub sits inward from the corner,
  // spokes run to a wall or to radius R, and the spiral's turns have a
  // little jitter per spoke. phase holds each spiral point's tremble phase.
  function geometry(R, seed) {
    const rand = rng(seed);
    const hub = 0.3 * R;
    const n = 14;
    const turns = 15;
    const start = rand() * Math.PI;
    const spokes = [];
    for (let i = 0; i < n; i++) {
      const a = start + (i * 2 * Math.PI) / n + (rand() - 0.5) * 0.18;
      const dx = Math.cos(a);
      const dy = Math.sin(a);
      let len = R * (0.86 + 0.14 * rand());
      if (dx < 0) len = Math.min(len, hub / -dx); // moored on a wall
      if (dy < 0) len = Math.min(len, hub / -dy);
      spokes.push({ dx, dy, len });
    }
    const ring = [];
    for (let k = 1; k <= turns; k++) {
      const base = R * (0.05 + 0.95 * Math.pow(k / turns, 1.2));
      ring.push({ base, r: spokes.map(() => base * (1 + (rand() - 0.5) * 0.05)), phase: spokes.map(() => rand() * 2 * Math.PI) });
    }
    return { R, hub, spokes, ring };
  }

  // drawWeb draws g at spin progress p (0..1: the spokes one by one, then
  // the spiral turn by turn from the hub outward, strand by strand), with
  // each spiral point moved along its spoke by up to amp px of tremble at
  // time t (s). Strands fade from aMax at the hub to aMin at the rim.
  function drawWeb(ctx, g, rgb, aMax, aMin, p, amp, t) {
    const { R, hub, spokes, ring } = g;
    const n = spokes.length;
    const shake = (k, i) => (amp ? amp * Math.sin(2 * Math.PI * TREMBLE_HZ * t + ring[k].phase[i]) * (0.4 + (0.6 * ring[k].base) / R) : 0);
    const rad = (k, i) => ring[k].r[i] + shake(k, i);
    const spokeShown = (i) => Math.min(Math.max((p / 0.35) * n - i, 0), 1) * spokes[i].len;
    const turnsShown = (Math.max(0, p - 0.35) / 0.65) * ring.length;
    ctx.lineWidth = 1;
    ctx.lineCap = "round";
    ctx.strokeStyle = "rgb(" + rgb + ")";
    let lo = 0;
    for (let k = 0; k <= ring.length; k++) {
      const hi = k < ring.length ? ring[k].base : R;
      ctx.beginPath();
      for (let i = 0; i < n; i++) {
        const s = spokes[i];
        const shown = spokeShown(i);
        const from = k > 0 ? rad(k - 1, i) : 0;
        const to = Math.min(k < ring.length ? rad(k, i) : s.len, shown, s.len);
        if (to <= from) continue;
        ctx.moveTo(hub + s.dx * from, hub + s.dy * from);
        ctx.lineTo(hub + s.dx * to, hub + s.dy * to);
      }
      if (k < ring.length) {
        const strands = Math.floor(Math.min(Math.max(turnsShown - k, 0), 1) * n);
        for (let i = 0; i < strands; i++) {
          const j = (i + 1) % n;
          const ra = rad(k, i);
          const rb = rad(k, j);
          if (spokes[i].len < ra || spokes[j].len < rb) continue;
          const ax = hub + spokes[i].dx * ra;
          const ay = hub + spokes[i].dy * ra;
          const bx = hub + spokes[j].dx * rb;
          const by = hub + spokes[j].dy * rb;
          const mx = (ax + bx) / 2;
          const my = (ay + by) / 2;
          ctx.moveTo(ax, ay);
          ctx.quadraticCurveTo(mx + (hub - mx) * 0.12, my + (hub - my) * 0.12, bx, by);
        }
      }
      ctx.globalAlpha = aMax - (aMax - aMin) * Math.min((lo + hi) / 2 / R, 1);
      ctx.stroke();
      lo = hi;
    }
    ctx.globalAlpha = 1;
  }

  function spinProgress(now) {
    return spinStart ? Math.min(1, (now - spinStart) / SPIN_MS) : 1;
  }

  // paint redraws every web for the current spin progress and tremble.
  function paint(now) {
    const css = getComputedStyle(root);
    const rgb = css.getPropertyValue("--web-rgb").trim();
    const aMax = parseFloat(css.getPropertyValue("--web-alpha-max"));
    const aMin = parseFloat(css.getPropertyValue("--web-alpha-min"));
    const p = still() ? 1 : spinProgress(now);
    for (const w of webs) {
      const ctx = w.ctx;
      ctx.setTransform(1, 0, 0, 1, 0, 0);
      ctx.clearRect(0, 0, w.canvas.width, w.canvas.height);
      ctx.setTransform(w.m[0], 0, 0, w.m[1], w.m[2], w.m[3]);
      drawWeb(ctx, w.geo, rgb, aMax, aMin, p, still() ? 0 : w.amp, now / 1000);
    }
    frames++;
  }

  // layout sizes every web canvas for the viewport (and redraws).
  function layout() {
    const dpr = Math.min(window.devicePixelRatio || 1, 1.5);
    const base = Math.min(420, Math.max(180, 0.42 * Math.min(window.innerWidth, window.innerHeight)));
    for (const w of webs) {
      const R = base * w.spec.scale;
      const size = Math.ceil(1.3 * R) + 12; // room for the tremble
      const right = w.spec.corner[1] === "r";
      const bottom = w.spec.corner[0] === "b";
      w.canvas.style.width = w.canvas.style.height = size + "px";
      w.canvas.width = w.canvas.height = Math.round(size * dpr);
      // Mirror the corner frame into this canvas's corner.
      w.m = [right ? -dpr : dpr, bottom ? -dpr : dpr, right ? w.canvas.width : 0, bottom ? w.canvas.height : 0];
      w.geo = geometry(R, w.spec.seed);
    }
    paint(performance.now());
  }

  function buildWebs() {
    const layer = $("webs");
    for (const spec of WEBS) {
      const canvas = document.createElement("canvas");
      canvas.style[spec.corner[1] === "r" ? "right" : "left"] = "0";
      canvas.style[spec.corner[0] === "b" ? "bottom" : "top"] = "0";
      layer.appendChild(canvas);
      webs.push({ canvas, ctx: canvas.getContext("2d"), spec, geo: null, m: null, amp: 0, target: 0 });
    }
  }

  function autoCalm() {
    return run.active && run.real && !override;
  }

  function calm() {
    return userCalm || autoCalm();
  }

  function still() {
    return calm() || reduce.matches || document.hidden;
  }

  // moving: something to animate (spinning, or a tremble to show or fade).
  function moving(now) {
    if (still()) return false;
    return spinProgress(now) < 1 || webs.some((w) => w.target > 0 || w.amp > 0.05);
  }

  function frame(now) {
    raf = 0;
    for (const w of webs) {
      w.amp += (w.target - w.amp) * 0.15;
      if (w.target === 0 && w.amp < 0.05) w.amp = 0;
    }
    paint(now);
    schedule();
  }

  function schedule() {
    if (!timer && !raf && moving(performance.now())) {
      timer = setTimeout(() => {
        timer = 0;
        raf = requestAnimationFrame(frame);
      }, FRAME_MS);
    }
  }

  function stop() {
    clearTimeout(timer);
    timer = 0;
    if (raf) cancelAnimationFrame(raf);
    raf = 0;
  }

  // refresh applies Calm, reduced motion and visibility: the switch, the
  // root class, the "ts:calm" event (spider3d.js) and the web loop.
  function refresh() {
    const c = calm();
    root.classList.toggle("calm", c);
    els.calm.setAttribute("aria-checked", c ? "true" : "false");
    const auto = autoCalm();
    if (auto) els.calm.setAttribute("aria-describedby", "calm-note");
    else els.calm.removeAttribute("aria-describedby");
    els.note.hidden = !auto;
    if (c !== lastCalm) {
      lastCalm = c;
      document.dispatchEvent(new CustomEvent("ts:calm"));
    }
    const s = still();
    if (s) stop();
    // Entering or leaving stillness: one clean frame (a finished web, no
    // tremble); otherwise the loop draws.
    if (s !== wasStill && !document.hidden && webs.length && webs[0].geo) paint(performance.now());
    wasStill = s;
    if (!s) schedule();
  }

  function label() {
    const text = TS.theme.current() === "dark" ? "Switch to light theme" : "Switch to dark theme";
    els.toggle.setAttribute("aria-label", text);
    els.toggle.title = text;
  }

  // crossfade runs change (the theme switch) inside the comic layer's
  // 250 ms red/cyan glitch; without it, as a 300 ms crossfade (a view
  // transition where the browser has them, else colour transitions).
  function crossfade(change) {
    if (TS.Comic) {
      TS.Comic.glitch(change);
      return;
    }
    if (document.startViewTransition) {
      document.startViewTransition(change);
      return;
    }
    root.classList.add("theme-fade");
    change();
    setTimeout(() => root.classList.remove("theme-fade"), FADE_MS);
  }

  // toggleTheme switches the theme: with the 3D spider when it is on
  // (spider3d.js), else with the SVG spider dropping on its thread; under
  // Calm or reduced motion at once.
  function toggleTheme() {
    if (busy) return;
    const next = TS.theme.current() === "dark" ? "light" : "dark";
    if (calm() || reduce.matches) {
      TS.theme.choose(next);
      return;
    }
    busy = true;
    const s3d = TS.Spider3D;
    if (s3d && s3d.ready()) {
      s3d.toggle(next, () => crossfade(() => TS.theme.choose(next))).then(() => {
        busy = false;
      });
      return;
    }
    els.toggle.classList.add("drop");
    setTimeout(() => {
      crossfade(() => TS.theme.choose(next));
      setTimeout(() => {
        els.toggle.classList.remove("drop");
        setTimeout(() => {
          busy = false;
        }, CLIMB_MS);
      }, FADE_MS);
    }, DROP_MS);
  }

  // onCalmClick turns Calm off (also when it came on for a run) or on.
  function onCalmClick() {
    if (calm()) {
      userCalm = false;
      if (autoCalm()) override = true;
    } else {
      userCalm = true;
    }
    try {
      localStorage.setItem(CALM_KEY, userCalm ? "1" : "0");
    } catch (e) {
      // storage blocked: the choice lasts until the page reloads
    }
    refresh();
  }

  function init() {
    els = { toggle: $("theme-toggle"), calm: $("calm-toggle"), note: $("calm-note") };
    try {
      userCalm = localStorage.getItem(CALM_KEY) === "1";
    } catch (e) {
      userCalm = false;
    }
    buildWebs();
    if (!still()) spinStart = performance.now();
    layout();
    label();
    refresh();
    els.toggle.addEventListener("click", toggleTheme);
    els.calm.addEventListener("click", onCalmClick);
    document.addEventListener("ts:theme", () => {
      paint(performance.now());
      label();
    });
    document.addEventListener("visibilitychange", refresh);
    if (reduce.addEventListener) reduce.addEventListener("change", refresh);
    window.addEventListener("resize", () => {
      clearTimeout(resizeTimer);
      resizeTimer = setTimeout(layout, 150);
    });
  }

  // setActivity gets the run state from app.js: whether an A/B run is
  // active, whether its replicas are real, and each stack's current request
  // rate (requests/s, all tenants).
  function setActivity(a) {
    const active = !!a.running;
    if (!active) override = false;
    run = { active, real: !!a.real };
    const rates = a.rates || {};
    for (const w of webs) {
      const rps = active ? rates[w.spec.stack] || 0 : 0;
      w.target = Math.min(AMP_MAX_PX, AMP_PX_PER_RPS * rps);
    }
    refresh();
  }

  init();
  return {
    setActivity,
    isCalm: calm,
    // For checks (check-theme.ps1): frames painted and current amplitudes.
    frames: () => frames,
    amplitudes: () => webs.map((w) => ({ stack: w.spec.stack, target: w.target, amp: w.amp })),
  };
})();
