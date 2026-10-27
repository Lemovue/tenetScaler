// Contrast check of the dashboard's spider-web theme. Reads the colour
// tokens of both themes from static/style.css and prints the WCAG 2.x
// contrast ratios as Markdown (pasted into THEME_NOTES.md). Exits 1 when a
// pair is below its minimum or the two light-token blocks differ.
//
//   node internal/web/contrast.mjs
//
// Text sits on plates (cards, header, banners): the worst case for a plate
// is a web strand at full strength behind it, so plate colours are checked
// both with and without that strand.
import { readFileSync } from "node:fs";

const css = readFileSync(new URL("./static/style.css", import.meta.url), "utf8");

function block(selector) {
  const start = css.indexOf(selector + " {");
  if (start < 0) throw new Error("no block " + selector);
  const body = css.slice(start + selector.length + 2, css.indexOf("}", start));
  const tokens = {};
  for (const m of body.matchAll(/--([\w-]+):\s*([^;]+);/g)) tokens[m[1]] = m[2].trim();
  return tokens;
}

const dark = block(":root");
const light = block(':root[data-theme="light"]');
const fallback = block(":root:not([data-theme])");
const same = JSON.stringify(light) === JSON.stringify(fallback);

// parse returns [r, g, b, a] with r, g, b in 0..255.
function parse(v) {
  let m = /^#([0-9a-f]{6})$/i.exec(v);
  if (m) return [0, 2, 4].map((i) => parseInt(m[1].slice(i, i + 2), 16)).concat(1);
  m = /^rgba?\(([^)]+)\)$/.exec(v);
  if (m) {
    const p = m[1].split(",").map(Number);
    return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1];
  }
  m = /^(\d+),\s*(\d+),\s*(\d+)$/.exec(v); // --web-rgb
  if (m) return [+m[1], +m[2], +m[3], 1];
  throw new Error("cannot parse colour " + v);
}

// over composites colour c (with alpha a) over the opaque base.
function over(c, a, base) {
  return [0, 1, 2].map((i) => a * c[i] + (1 - a) * base[i]).concat(1);
}

function lum(c) {
  const l = c.slice(0, 3).map((x) => {
    const s = x / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * l[0] + 0.7152 * l[1] + 0.0722 * l[2];
}

function ratio(a, b) {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

// lab converts an sRGB colour to CIELAB (D65).
function lab(c) {
  const [r, g, b] = c.slice(0, 3).map((x) => {
    const s = x / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  const f = (t) => (t > 216 / 24389 ? Math.cbrt(t) : (24389 / 27 * t + 16) / 116);
  const fx = f((0.4124 * r + 0.3576 * g + 0.1805 * b) / 0.95047);
  const fy = f(0.2126 * r + 0.7152 * g + 0.0722 * b);
  const fz = f((0.0193 * r + 0.1192 * g + 0.9505 * b) / 1.08883);
  return [116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz)];
}

// de2000 is the CIEDE2000 colour difference.
function de2000(c1, c2) {
  const [L1, a1, b1] = lab(c1);
  const [L2, a2, b2] = lab(c2);
  const rad = Math.PI / 180;
  const Cb = (Math.hypot(a1, b1) + Math.hypot(a2, b2)) / 2;
  const G = 0.5 * (1 - Math.sqrt(Cb ** 7 / (Cb ** 7 + 25 ** 7)));
  const a1p = (1 + G) * a1;
  const a2p = (1 + G) * a2;
  const C1p = Math.hypot(a1p, b1);
  const C2p = Math.hypot(a2p, b2);
  const h = (a, b) => ((Math.atan2(b, a) / rad) + 360) % 360;
  const h1p = h(a1p, b1);
  const h2p = h(a2p, b2);
  let dhp = h2p - h1p;
  if (C1p * C2p === 0) dhp = 0;
  else if (dhp > 180) dhp -= 360;
  else if (dhp < -180) dhp += 360;
  const dLp = L2 - L1;
  const dCp = C2p - C1p;
  const dHp = 2 * Math.sqrt(C1p * C2p) * Math.sin((dhp / 2) * rad);
  const Lbp = (L1 + L2) / 2;
  const Cbp = (C1p + C2p) / 2;
  let hbp = h1p + h2p;
  if (C1p * C2p !== 0) hbp = Math.abs(h1p - h2p) <= 180 ? hbp / 2 : hbp < 360 ? (hbp + 360) / 2 : (hbp - 360) / 2;
  const T = 1 - 0.17 * Math.cos((hbp - 30) * rad) + 0.24 * Math.cos(2 * hbp * rad) +
    0.32 * Math.cos((3 * hbp + 6) * rad) - 0.2 * Math.cos((4 * hbp - 63) * rad);
  const Rc = 2 * Math.sqrt(Cbp ** 7 / (Cbp ** 7 + 25 ** 7));
  const Rt = -Math.sin(2 * 30 * Math.exp(-(((hbp - 275) / 25) ** 2)) * rad) * Rc;
  const Sl = 1 + (0.015 * (Lbp - 50) ** 2) / Math.sqrt(20 + (Lbp - 50) ** 2);
  const Sc = 1 + 0.045 * Cbp;
  const Sh = 1 + 0.015 * Cbp * T;
  return Math.sqrt((dLp / Sl) ** 2 + (dCp / Sc) ** 2 + (dHp / Sh) ** 2 + Rt * (dCp / Sc) * (dHp / Sh));
}

// CVD simulates colour-vision deficiency (Machado et al. 2009, severity 1)
// in linear RGB.
const CVD = {
  protan: [[0.152286, 1.052583, -0.204868], [0.114503, 0.786281, 0.099216], [-0.003882, -0.048116, 1.051998]],
  deutan: [[0.367322, 0.860646, -0.227968], [0.280085, 0.672501, 0.047413], [-0.01182, 0.04294, 0.968881]],
  tritan: [[1.255528, -0.076749, -0.178779], [-0.078411, 0.930809, 0.147602], [0.004733, 0.691367, 0.3039]],
};

function simulate(c, kind) {
  if (!CVD[kind]) return c;
  const lin = c.slice(0, 3).map((x) => {
    const s = x / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  });
  return CVD[kind].map((row) => {
    const v = Math.min(Math.max(row[0] * lin[0] + row[1] * lin[1] + row[2] * lin[2], 0), 1);
    return 255 * (v <= 0.0031308 ? 12.92 * v : 1.055 * v ** (1 / 2.4) - 0.055);
  }).concat(1);
}

// The theme's decoration colours: the spider of each theme.
const THEME_RED = parse(light.spider);
const THEME_BLUE = parse(dark.spider);

const hex = (c) => "#" + c.slice(0, 3).map((x) => Math.round(x).toString(16).padStart(2, "0")).join("");

function check(name, t) {
  const col = (k) => parse(t[k]);
  const page = col("page");
  const plate = col("plate");
  const strand = over(col("web-rgb"), Number(t["web-alpha-max"]), page);
  const plateClean = over(plate, plate[3], page);
  const plateWorst = over(plate, plate[3], strand);
  const webBehindPlate = Number(t["web-alpha-max"]) * (1 - plate[3]);
  const rows = [];
  let ok = true;
  // pair: the worse of the given backgrounds counts.
  const pair = (what, fg, bgs, min) => {
    const f = typeof fg === "string" ? col(fg) : fg;
    const worst = Math.min(...bgs.map(([, b]) => ratio(f, b)));
    const pass = worst >= min;
    if (!pass) ok = false;
    rows.push([what, bgs.map(([n]) => n).join(" / "), worst.toFixed(2), min, pass ? "pass" : "FAIL"]);
  };
  const onPlate = [["card (no web)", plateClean], ["card + web strand", plateWorst]];
  const onPage = [["page", page]];
  pair("text (--ink)", "ink", onPage.concat(onPlate, [["--surface", col("surface")], ["--surface-2", col("surface-2")]]), 4.5);
  pair("muted text (--ink-2)", "ink-2", onPage.concat(onPlate, [["--surface", col("surface")]]), 4.5);
  pair("inverted pills (tab, kicker, REAL, live banner)", "invert-ink", [["--ink", col("ink")]], 4.5);
  pair("SIMULATED badge", "sim-ink", [["--sim-bg", col("sim-bg")]], 4.5);
  pair("INJECTED badge (dashed)", "ink", onPlate, 4.5);
  pair("errors", "error-ink", [["--error-bg", col("error-bg")]].concat(onPlate), 4.5);
  pair("command in the offline banner (code)", "error-ink", [["--surface-2", col("surface-2")]], 4.5);
  // Comic layer: text over a halftone dot (on a card with a web strand
  // behind it), and the caption boxes.
  const dot = col("halftone");
  const onDot = [["halftone dot on card + web", over(dot, dot[3], plateWorst)], ["halftone dot on page", over(dot, dot[3], page)]];
  pair("headings and text over the halftone (--ink)", "ink", onDot, 4.5);
  pair("caption box text", "caption-ink", [["--caption-bg", col("caption-bg")]], 4.5);
  pair("caption box outline (graphic)", "caption-ink", [["--caption-bg", col("caption-bg")]], 3);
  for (const s of ["met", "missed", "shed"]) pair("status " + s.toUpperCase() + " (--" + s + ")", s, onPlate, 4.5);
  pair("spider toggle (graphic)", "spider", onPage, 3);
  pair("focus ring (graphic)", "focus", onPage.concat(onPlate), 3);
  for (let i = 1; i <= 8; i++) pair("chart line --series-" + i, "series-" + i, onPlate, 3);
  for (const k of ["rep-ready", "rep-starting", "threshold"]) pair("chart line --" + k, k, onPlate, 3);
  const lines = [];
  lines.push("### " + name + " theme");
  lines.push("");
  lines.push("Page " + t.page + "; card plate " + t.plate + " = " + hex(plateClean) + ", or " + hex(plateWorst) +
    " with a web strand at full strength (" + t["web-alpha-max"] + ") behind it. A web behind a card or chart shows at " +
    (webBehindPlate * 100).toFixed(1) + "% at most (limit 15%).");
  lines.push("");
  lines.push("| Pair | On | Worst ratio | Minimum | |");
  lines.push("|---|---|---|---|---|");
  for (const r of rows) lines.push("| " + r.join(" | ") + " |");
  // Distinctness (CIEDE2000): status colours vs the theme red and blue, and
  // tenants A, B, C (slots 1-3) for normal and simulated CVD vision.
  const dist = [];
  const far = (what, a, b, min) => {
    const d = de2000(a, b);
    if (d < min) ok = false;
    dist.push([what, d.toFixed(1), min, d >= min ? "pass" : "FAIL"]);
  };
  for (const s of ["met", "missed", "shed"]) {
    far("--" + s + " vs theme red " + light.spider, col(s), THEME_RED, 20);
    far("--" + s + " vs theme blue " + dark.spider, col(s), THEME_BLUE, 20);
  }
  for (const kind of ["normal", "protan", "deutan", "tritan"]) {
    for (const [i, j] of [[1, 2], [1, 3], [2, 3]]) {
      far("series " + i + " vs " + j + " (" + kind + ")", simulate(col("series-" + i), kind),
        simulate(col("series-" + j), kind), kind === "normal" ? 15 : 8);
    }
  }
  lines.push("");
  lines.push("| Distinctness (CIEDE2000) | ΔE | Minimum | |");
  lines.push("|---|---|---|---|");
  for (const r of dist) lines.push("| " + r.join(" | ") + " |");
  if (webBehindPlate > 0.15) ok = false;
  return { ok, text: lines.join("\n") };
}

const out = [check("Dark", dark), check("Light", light)];
console.log(out.map((o) => o.text).join("\n\n"));
console.log("\nLight tokens identical in the data-theme and no-script blocks: " + (same ? "yes" : "NO"));
process.exit(out.every((o) => o.ok) && same ? 0 : 1);
