// Canvas line chart of the dashboard: thin 2px lines (solid, dashed or
// dotted, so a series never relies on colour alone) on a shared x axis
// (seconds since the stack's traffic started), solid hairline grid, dashed
// threshold lines (SLO, budget), an optional shaded window (the spike),
// direct labels at line ends, and a crosshair with a tooltip that follows
// the pointer or the arrow keys when the canvas has focus. Keyboard moves
// are also announced through a polite live region.
"use strict";

window.TS = window.TS || {};

(function () {
  const PAD = { left: 46, right: 34, top: 14, bottom: 24 };
  const FONT = '12px system-ui, -apple-system, "Segoe UI", sans-serif';

  // niceStep picks a 1-2-5 step giving about target intervals over range.
  function niceStep(range, target) {
    if (!(range > 0)) return 1;
    const raw = range / target;
    const p = Math.pow(10, Math.floor(Math.log10(raw)));
    const f = raw / p;
    return (f < 1.5 ? 1 : f < 3 ? 2 : f < 7 ? 5 : 10) * p;
  }

  function fmtTick(v) {
    return Math.abs(v - Math.round(v)) < 1e-9 ? String(Math.round(v)) : v.toFixed(1);
  }

  // dashPattern is the canvas dash of a series' dash option: true
  // (dashed), "dot" (dotted) or none (solid).
  function dashPattern(dash) {
    if (dash === "dot") return [2, 3];
    return dash ? [5, 4] : [];
  }

  // keyClass is the legend/tooltip key class matching dashPattern.
  function keyClass(dash) {
    if (dash === "dot") return "key key-dot";
    return dash ? "key key-dash" : "key";
  }

  class LineChart {
    // opts: {title, yFormat(v) -> string}
    constructor(canvas, opts) {
      this.canvas = canvas;
      this.opts = opts;
      this.data = null;
      this.hover = -1;
      this.tip = TS.util.el("div", { class: "tooltip", hidden: true });
      canvas.parentElement.appendChild(this.tip);
      // The canvas handles its own arrow keys, so screen readers pass them
      // through (application role); each move is announced in this.live.
      this.live = TS.util.el("p", { class: "visually-hidden", "aria-live": "polite" });
      canvas.parentElement.appendChild(this.live);
      canvas.setAttribute("role", "application");
      canvas.setAttribute("aria-roledescription", "chart");
      canvas.addEventListener("pointermove", (e) => this.onPointer(e));
      canvas.addEventListener("pointerleave", () => this.setHover(-1));
      canvas.addEventListener("blur", () => this.setHover(-1));
      canvas.addEventListener("keydown", (e) => this.onKey(e));
      if (window.ResizeObserver) new ResizeObserver(() => this.draw()).observe(canvas);
      const mq = window.matchMedia("(prefers-color-scheme: dark)");
      if (mq.addEventListener) mq.addEventListener("change", () => this.draw());
      this.updateAria();
    }

    // update replaces the data: {xs, xMax, yMax, series: [{name, colorVar,
    // ys, step, dash}], thresholds: [{y, label}], band: {x0, x1, label},
    // empty: text shown without data}.
    update(data) {
      this.data = data;
      if (this.hover >= data.xs.length) this.hover = data.xs.length - 1;
      this.draw();
      this.updateAria();
      if (this.hover >= 0) this.showTip();
    }

    geometry() {
      const w = this.canvas.clientWidth;
      const h = this.canvas.clientHeight;
      const d = this.data;
      const xMax = Math.max(d.xMax || 1, 1);
      const yMax = d.yMax > 0 ? d.yMax : 1;
      const pw = w - PAD.left - PAD.right;
      const ph = h - PAD.top - PAD.bottom;
      return {
        w, h, xMax, yMax,
        x: (v) => PAD.left + (v / xMax) * pw,
        y: (v) => PAD.top + ph - (Math.min(v, yMax) / yMax) * ph,
      };
    }

    draw() {
      const c = this.canvas;
      const w = c.clientWidth;
      const h = c.clientHeight;
      if (!w || !h || !this.data) return; // hidden: redrawn when shown
      const dpr = window.devicePixelRatio || 1;
      if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) {
        c.width = Math.round(w * dpr);
        c.height = Math.round(h * dpr);
      }
      const ctx = c.getContext("2d");
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      const color = (name) => TS.util.cssVar(c, name);
      ctx.fillStyle = color("--surface");
      ctx.fillRect(0, 0, w, h);
      ctx.font = FONT;
      const d = this.data;
      const g = this.geometry();
      this.drawBand(ctx, g, color);
      this.drawAxes(ctx, g, color);
      for (const t of d.thresholds || []) this.drawThreshold(ctx, g, color, t);
      if (!d.xs.length) {
        ctx.fillStyle = color("--ink-2");
        ctx.textAlign = "center";
        ctx.fillText(d.empty || "No data yet", PAD.left + (w - PAD.left - PAD.right) / 2, PAD.top + (h - PAD.top - PAD.bottom) / 2);
        return;
      }
      for (const s of d.series) this.drawSeries(ctx, g, color(s.colorVar), s);
      this.drawLabels(ctx, g, color);
      if (this.hover >= 0) this.drawCrosshair(ctx, g, color);
    }

    drawBand(ctx, g, color) {
      const b = this.data.band;
      if (!b || !(b.x1 > b.x0)) return;
      const x0 = g.x(b.x0);
      const x1 = g.x(Math.min(b.x1, g.xMax));
      ctx.fillStyle = color("--band");
      ctx.fillRect(x0, PAD.top, x1 - x0, g.h - PAD.top - PAD.bottom);
      ctx.fillStyle = color("--ink-2");
      ctx.textAlign = "left";
      ctx.textBaseline = "top";
      ctx.fillText(b.label, x0 + 4, PAD.top + 2);
    }

    drawAxes(ctx, g, color) {
      const yStep = niceStep(g.yMax, 4);
      ctx.lineWidth = 1;
      ctx.textBaseline = "middle";
      ctx.textAlign = "right";
      for (let v = 0; v <= g.yMax + 1e-9; v += yStep) {
        const y = Math.round(g.y(v)) + 0.5;
        ctx.strokeStyle = v === 0 ? color("--axis") : color("--grid");
        ctx.beginPath();
        ctx.moveTo(PAD.left, y);
        ctx.lineTo(g.w - PAD.right, y);
        ctx.stroke();
        ctx.fillStyle = color("--ink-2");
        ctx.fillText(this.opts.yTick ? this.opts.yTick(v) : fmtTick(v), PAD.left - 6, y);
      }
      const xStep = niceStep(g.xMax, Math.max(2, Math.floor((g.w - PAD.left - PAD.right) / 70)));
      ctx.textAlign = "center";
      ctx.textBaseline = "top";
      for (let v = 0; v <= g.xMax + 1e-9; v += xStep) {
        ctx.fillText(fmtTick(v) + " s", g.x(v), g.h - PAD.bottom + 6);
      }
    }

    drawThreshold(ctx, g, color, t) {
      if (!(t.y >= 0) || t.y > g.yMax) return;
      const y = Math.round(g.y(t.y)) + 0.5;
      ctx.strokeStyle = color("--threshold");
      ctx.lineWidth = 1.5;
      ctx.setLineDash([6, 4]);
      ctx.beginPath();
      ctx.moveTo(PAD.left, y);
      ctx.lineTo(g.w - PAD.right, y);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = color("--ink-2");
      ctx.textAlign = "left"; // the right end holds the series' direct labels
      ctx.textBaseline = "bottom";
      ctx.fillText(t.label, PAD.left + 4, y - 2);
    }

    drawSeries(ctx, g, stroke, s) {
      const xs = this.data.xs;
      ctx.strokeStyle = stroke;
      ctx.lineWidth = 2;
      ctx.lineJoin = "round";
      ctx.setLineDash(dashPattern(s.dash));
      ctx.beginPath();
      let open = false;
      let prevY = 0;
      for (let i = 0; i < xs.length; i++) {
        const v = s.ys[i];
        if (v === null || v === undefined || !Number.isFinite(v)) {
          open = false;
          continue;
        }
        const x = g.x(xs[i]);
        const y = g.y(v);
        if (!open) ctx.moveTo(x, y);
        else if (s.step) {
          ctx.lineTo(x, prevY);
          ctx.lineTo(x, y);
        } else ctx.lineTo(x, y);
        open = true;
        prevY = y;
      }
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // drawLabels writes each series' name at its last point, nudged apart
    // and kept inside the plot (a label too wide for the right margin moves
    // left, above its line).
    drawLabels(ctx, g, color) {
      const xs = this.data.xs;
      const labels = [];
      for (const s of this.data.series) {
        for (let i = xs.length - 1; i >= 0; i--) {
          const v = s.ys[i];
          if (v !== null && v !== undefined && Number.isFinite(v)) {
            labels.push({ text: s.label || s.name, x: g.x(xs[i]) + 6, y: g.y(v) });
            break;
          }
        }
      }
      for (const l of labels) {
        const tw = ctx.measureText(l.text).width;
        if (l.x + tw > g.w - 2) {
          l.x = g.w - 2 - tw;
          l.y -= 9;
        }
      }
      labels.sort((a, b) => a.y - b.y);
      const gap = 13;
      for (let i = 1; i < labels.length; i++) labels[i].y = Math.max(labels[i].y, labels[i - 1].y + gap);
      const bottom = g.h - PAD.bottom - 6;
      for (let i = labels.length - 1; i >= 0; i--) {
        const limit = i === labels.length - 1 ? bottom : labels[i + 1].y - gap;
        labels[i].y = Math.max(PAD.top + 6, Math.min(labels[i].y, limit));
      }
      ctx.fillStyle = color("--ink");
      ctx.textAlign = "left";
      ctx.textBaseline = "middle";
      for (const l of labels) ctx.fillText(l.text, l.x, l.y);
    }

    drawCrosshair(ctx, g, color) {
      const i = this.hover;
      const x = Math.round(g.x(this.data.xs[i])) + 0.5;
      ctx.strokeStyle = color("--ink-2");
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(x, PAD.top);
      ctx.lineTo(x, g.h - PAD.bottom);
      ctx.stroke();
      for (const s of this.data.series) {
        const v = s.ys[i];
        if (v === null || v === undefined || !Number.isFinite(v)) continue;
        ctx.beginPath();
        ctx.arc(x, g.y(v), 4, 0, 2 * Math.PI);
        ctx.fillStyle = color(s.colorVar);
        ctx.strokeStyle = color("--surface");
        ctx.lineWidth = 2;
        ctx.fill();
        ctx.stroke();
      }
    }

    onPointer(e) {
      if (!this.data || !this.data.xs.length) return;
      const g = this.geometry();
      const px = e.clientX - this.canvas.getBoundingClientRect().left;
      let best = 0;
      for (let i = 1; i < this.data.xs.length; i++) {
        if (Math.abs(g.x(this.data.xs[i]) - px) < Math.abs(g.x(this.data.xs[best]) - px)) best = i;
      }
      this.setHover(best);
    }

    onKey(e) {
      const n = this.data ? this.data.xs.length : 0;
      if (!n) return;
      const keys = { ArrowLeft: -1, ArrowRight: 1, PageDown: -10, PageUp: 10 };
      let i = this.hover;
      if (e.key in keys) i = i < 0 ? n - 1 : Math.min(n - 1, Math.max(0, i + keys[e.key]));
      else if (e.key === "Home") i = 0;
      else if (e.key === "End") i = n - 1;
      else if (e.key === "Escape") i = -1;
      else return;
      e.preventDefault();
      this.setHover(i);
      TS.util.setText(this.live, i < 0 ? "Cursor hidden." : this.describe(i));
    }

    // describe reads the values at index i in words, as the tooltip shows
    // them: "t = 42 s: A 1.20 s, B 850 ms, C no data".
    describe(i) {
      const d = this.data;
      return "t = " + fmtTick(d.xs[i]) + " s: " + d.series.map((s) => (s.label || s.name) + " " + this.value(s.ys[i])).join(", ");
    }

    value(v) {
      return v === null || v === undefined || !Number.isFinite(v) ? "no data" : this.opts.yFormat(v);
    }

    setHover(i) {
      this.hover = i;
      this.draw();
      if (i < 0) this.tip.hidden = true;
      else this.showTip();
    }

    showTip() {
      const d = this.data;
      const i = this.hover;
      const el = TS.util.el;
      TS.util.clear(this.tip);
      this.tip.appendChild(el("div", { class: "tt-head" }, "t = " + fmtTick(d.xs[i]) + " s"));
      for (const s of d.series) {
        const key = el("span", { class: keyClass(s.dash) });
        key.style.borderTopColor = "var(" + s.colorVar + ")";
        this.tip.appendChild(el("div", { class: "tt-row" }, [key, el("strong", {}, this.value(s.ys[i])), s.label || s.name]));
      }
      this.tip.hidden = false;
      const g = this.geometry();
      const x = g.x(d.xs[i]);
      const left = x + 12 + this.tip.offsetWidth > g.w ? x - 12 - this.tip.offsetWidth : x + 12;
      this.tip.style.left = Math.max(0, left) + "px";
      this.tip.style.top = PAD.top + "px";
    }

    // updateAria describes the latest values, so screen readers get the
    // same information as the lines (the tables below hold the details).
    updateAria() {
      const d = this.data;
      let text = this.opts.title + ": no data yet.";
      if (d && d.xs.length) {
        const last = d.xs.length - 1;
        const parts = d.series.map((s) => (s.label || s.name) + " " + this.value(s.ys[last]));
        text = this.opts.title + " at " + fmtTick(d.xs[last]) + " s: " + parts.join(", ") +
          ". The left and right arrow keys, Home and End read the values at other times.";
      }
      this.canvas.setAttribute("aria-label", text);
    }
  }

  // renderLegend fills ul with one entry per series: a line key (dashed or
  // dotted like the series) in the series colour and the name.
  function renderLegend(ul, items) {
    TS.util.clear(ul);
    for (const it of items) {
      const key = TS.util.el("span", { class: keyClass(it.dash), "aria-hidden": "true" });
      key.style.borderTopColor = "var(" + it.colorVar + ")";
      ul.appendChild(TS.util.el("li", {}, [key, it.name]));
    }
  }

  TS.LineChart = LineChart;
  TS.renderLegend = renderLegend;
})();
