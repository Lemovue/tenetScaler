// 3D decoration: one original spider, modelled after real jumping spiders
// and orb-weavers, shown twice through one shared WebGL renderer (one
// canvas over the page, each view scissored to its DOM anchor): in detail
// on the hero's orb web, and at 40 px with reduced detail (no hairs, fewer
// polygons) as the theme toggle. three.js r170 (vendor/three.module.min.js,
// MIT); every texture is generated at runtime.
//
// Anatomy: cephalothorax with a raised carapace, eight eyes (two large
// glossy front eyes), chelicerae with fangs, pedipalps, pedicel, a tapered
// abdomen with spinnerets, and eight legs of four visible segments (femur,
// patella+tibia, metatarsus, tarsus) whose knees arch above the body. One
// theme hue, a darker shade of it at the joints, fangs and eyes; no stripes,
// patterns or emblems.
//
// Frames are drawn on demand: the toggle's drop and climb (and a rare leg
// twitch) at up to 30 a second; the hero spider's idle life (breathing, a
// pedipalp twitch, the head turning toward the pointer) at up to 15, only
// while the hero is on screen. Nothing is drawn while the tab is hidden or
// under Calm, where 2D SVG spiders take over (as without WebGL); under
// prefers-reduced-motion the 3D stays still. Device pixel ratio at most 1.5.
import * as THREE from "./vendor/three.module.min.js";

const TS = window.TS;
const root = document.documentElement;
const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
const ACTIVE_MS = 1000 / 30; // frame cap while something moves on purpose
const IDLE_MS = 1000 / 15; // frame cap of the hero spider's idle life
const MAX_DPR = 1.5;
// Toggle: drop, hold while the theme crossfades, climb back.
const DROP_MS = 380;
const HOLD_MS = 300;
const CLIMB_MS = 760;
const DROP_PX = 26;
const TOGGLE_PX = 40; // toggle spider height
const HERO_PX = 80; // hero spider height (80-100 px)
const HERO_OVERFLOW = 20; // px the hero view reaches above and below its slot (margins, no text)
const TILT = 0.07; // hero parallax, radians (about 4 degrees)
const MAX_HAIRS = 2000;

// Legs, pairs I to IV front to back: attachment along the cephalothorax,
// direction (degrees from sideways toward the front), total length, and
// the rest pose's femur lift and knee bend (radians).
const LEGS = [
  { y: 0.5, dir: 52, len: 6.7, lift: 0.95, knee: 2.05 },
  { y: 0.2, dir: 20, len: 6.2, lift: 1.0, knee: 2.1 },
  { y: -0.1, dir: -16, len: 4.9, lift: 1.05, knee: 2.1 },
  { y: -0.38, dir: -48, len: 6.0, lift: 0.95, knee: 2.0 },
];
const SEG = [0.3, 0.32, 0.24, 0.14]; // femur, patella+tibia, metatarsus, tarsus (share of length)
const SEG_R = [0.105, 0.085, 0.062, 0.043]; // base radius of each segment
const META = 0.35; // rest bend at the metatarsus joint
const TARSUS = 0.25; // rest bend at the tarsus joint
const FOLD = { lift: 1.45, knee: 2.6, meta: 0.6, tarsus: 0.4, yawIn: 0.55 }; // legs folded in (drop)
const CEPH = new THREE.Vector3(0, 0.45, 0.05); // carapace centre
const PIVOT = new THREE.Vector3(0, -0.55, 0.1); // the head turns about the pedicel
const ABDOMEN = 0.88; // abdomen size, relative to the profile in abdomenGeometry

let renderer = null;
let mats = null;
let toggleView = null;
let heroView = null;
let frames = 0;
let timer = 0;
let raf = 0;
let lastFrame = 0;
let busy = false;
let heroVisible = false;
let twitchTimer = 0;
let idleTwitch = true;
const anims = new Set();
const stats = { active: { n: 0, ms: 0 }, idle: { n: 0, ms: 0 } };
const pointer = { x: -1, y: -1 };
const tilt = { x: 0, y: 0, tx: 0, ty: 0 };

const lerp = (a, b, t) => a + (b - a) * t;
const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
const easeInOut = (t) => (t < 0.5 ? 2 * t * t : 1 - (-2 * t + 2) ** 2 / 2);
const easeOut = (t) => 1 - (1 - t) ** 3;

function css(name) {
  return getComputedStyle(root).getPropertyValue(name).trim();
}

// rng is a seeded generator (mulberry32), as in decor.js.
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

function calm() {
  return TS.Decor && TS.Decor.isCalm();
}

// spiderOn: the 3D toggle spider replaces the SVG one.
function spiderOn() {
  return !!renderer && !calm() && !reduce.matches;
}

// idleOn: the hero spider is alive (breathing, twitching, looking).
function idleOn() {
  return !!renderer && heroVisible && !calm() && !reduce.matches && !document.hidden && heroView.active();
}

// --- materials and textures ------------------------------------------------

// noiseTexture is a runtime value-noise bump map: a coarse bilinear grid
// plus fine grain, tiled.
function noiseTexture(size) {
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const ctx = c.getContext("2d");
  const img = ctx.createImageData(size, size);
  const rand = rng(97);
  const n = 16;
  const grid = [];
  for (let i = 0; i < (n + 1) * (n + 1); i++) grid.push(rand());
  const at = (gx, gy) => grid[(gy % n) * (n + 1) + (gx % n)];
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const fx = (x / size) * n;
      const fy = (y / size) * n;
      const ix = Math.floor(fx);
      const iy = Math.floor(fy);
      const tx = fx - ix;
      const ty = fy - iy;
      const v = lerp(lerp(at(ix, iy), at(ix + 1, iy), tx), lerp(at(ix, iy + 1), at(ix + 1, iy + 1), tx), ty);
      const g = Math.round(255 * (0.7 * v + 0.3 * rand()));
      const k = 4 * (y * size + x);
      img.data[k] = img.data[k + 1] = img.data[k + 2] = g;
      img.data[k + 3] = 255;
    }
  }
  ctx.putImageData(img, 0, 0);
  const tex = new THREE.CanvasTexture(c);
  tex.wrapS = tex.wrapT = THREE.RepeatWrapping;
  tex.repeat.set(3, 3);
  return tex;
}

// shades derives the palette from one theme hue: the body, a darker shade
// for joints and fangs, a near-black shade of it for the eyes, a lighter
// one for hairs and sheen.
function shades(hex) {
  const base = new THREE.Color(hex);
  const hsl = {};
  base.getHSL(hsl);
  const at = (s, l) => new THREE.Color().setHSL(hsl.h, clamp(hsl.s * s, 0, 1), clamp(hsl.l * l, 0, 1));
  return { body: base, joint: at(1, 0.5), eye: at(0.8, 0.16), hair: at(0.9, 0.78), sheen: at(0.6, 1.5) };
}

function makeMaterials() {
  const bump = noiseTexture(128);
  const chitin = () => new THREE.MeshPhysicalMaterial({
    roughness: 0.42, metalness: 0, clearcoat: 0.85, clearcoatRoughness: 0.3,
    sheen: 0.5, sheenRoughness: 0.6, bumpMap: bump, bumpScale: 0.6,
  });
  return {
    body: chitin(),
    joint: chitin(),
    eye: new THREE.MeshPhysicalMaterial({ roughness: 0.06, metalness: 0, clearcoat: 1, clearcoatRoughness: 0.03 }),
    hair: new THREE.MeshStandardMaterial({ roughness: 0.9, metalness: 0 }),
    thread: new THREE.LineBasicMaterial({ transparent: true, opacity: 0.7 }),
  };
}

// paint sets every material from a hue (a hex colour), or blends two hues.
function paint(from, to, t) {
  const a = shades(from);
  const b = to ? shades(to) : a;
  const k = to ? t : 0;
  mats.body.color.lerpColors(a.body, b.body, k);
  mats.body.sheenColor.lerpColors(a.sheen, b.sheen, k);
  mats.joint.color.lerpColors(a.joint, b.joint, k);
  mats.joint.sheenColor.lerpColors(a.sheen, b.sheen, k);
  mats.eye.color.lerpColors(a.eye, b.eye, k);
  mats.hair.color.lerpColors(a.hair, b.hair, k);
}

function spiderColor(theme) {
  return css(theme === "light" ? "--spider-light" : "--spider-dark");
}

function lights(scene) {
  scene.add(new THREE.AmbientLight(0xffffff, 0.35));
  const key = new THREE.DirectionalLight(0xffffff, 2.6);
  key.position.set(-2, 3, 4);
  const fill = new THREE.DirectionalLight(0xdde6ff, 0.9);
  fill.position.set(3, -1, 2);
  const rim = new THREE.DirectionalLight(0xffffff, 5.0); // the silhouette on jet black
  rim.position.set(0.5, 2, -4);
  scene.add(key, fill, rim);
}

// --- the model -------------------------------------------------------------

// The model's frame: x to the right, y to the front (anterior), z up (the
// back, dorsal). poseSpider() moves it; hang()/stand() place it.

function carapaceGeometry(ws, hs) {
  const g = new THREE.SphereGeometry(1, ws, hs);
  g.rotateZ(Math.PI / 2); // poles to the sides, where the legs hide them
  const p = g.attributes.position;
  for (let i = 0; i < p.count; i++) {
    const x = p.getX(i);
    const y = p.getY(i);
    const z = p.getZ(i);
    const front = Math.max(0, y);
    // Raised at the eye region, flat underneath (the sternum), narrower behind.
    p.setXYZ(i, x * (0.95 - 0.12 * Math.max(0, -y)), y * 1.2, z > 0 ? z * (0.6 + 0.25 * front) : z * 0.38);
  }
  g.computeVertexNormals();
  return g;
}

// abdomenGeometry is a lathe of the abdomen's profile (widest in front of
// the middle, tapering to the spinnerets), centred on its middle.
function abdomenGeometry(segments) {
  const prof = [[0, 0], [0.32, -0.05], [0.7, -0.25], [0.95, -0.6], [1.08, -1.05], [1.1, -1.5],
    [1.0, -2.0], [0.78, -2.45], [0.48, -2.8], [0.2, -3.02], [0, -3.08]];
  const g = new THREE.LatheGeometry(prof.map(([r, y]) => new THREE.Vector2(r, y + 1.54)), segments);
  g.scale(ABDOMEN, ABDOMEN, 0.82 * ABDOMEN);
  return g;
}

// taper is a tapered cylinder of length len along +x, base at the origin.
function taper(len, r0, r1, radial) {
  const g = new THREE.CylinderGeometry(r1, r0, len, radial, 1);
  g.rotateZ(-Math.PI / 2);
  g.translate(len / 2, 0, 0);
  return g;
}

// surface finds where a ray from far out along dir meets probe, a mesh at
// the origin with the carapace's geometry: the point and outward normal in
// the carapace's own frame.
function surface(probe, dir) {
  const d = dir.clone().normalize();
  const ray = new THREE.Raycaster(d.clone().multiplyScalar(6), d.clone().negate());
  const hit = ray.intersectObject(probe)[0];
  return hit ? { p: hit.point, n: hit.face.normal.clone() } : { p: d.clone().multiplyScalar(0.8), n: d };
}

// hairs adds n hair instances to parent, at points from place(i) -> {p,
// d} (position and hair direction), within the model's hair budget.
function hairs(parent, n, place, budget, hairGeo) {
  const k = Math.min(n, budget.left);
  if (k <= 0) return;
  budget.left -= k;
  const mesh = new THREE.InstancedMesh(hairGeo, mats.hair, k);
  const m = new THREE.Matrix4();
  const q = new THREE.Quaternion();
  const up = new THREE.Vector3(0, 1, 0);
  const s = new THREE.Vector3();
  for (let i = 0; i < k; i++) {
    const { p, d, len } = place(i);
    q.setFromUnitVectors(up, d.normalize());
    s.set(1, len, 1);
    mesh.setMatrixAt(i, m.compose(p, q, s));
  }
  parent.add(mesh);
}

// buildSpider builds the model; "high" detail has hairs and more polygons.
function buildSpider(detail) {
  const hi = detail === "high";
  const rand = rng(hi ? 7 : 8);
  const radial = hi ? 12 : 6;
  const budget = { left: hi ? MAX_HAIRS : 0 };
  const hairGeo = new THREE.ConeGeometry(0.016, 0.2, 3, 1).translate(0, 0.1, 0);
  const joint = new THREE.SphereGeometry(1, hi ? 12 : 6, hi ? 8 : 4);
  const ball = (parent, r, p, mat) => {
    const b = new THREE.Mesh(joint, mat || mats.joint);
    b.scale.setScalar(r);
    if (p) b.position.copy(p);
    parent.add(b);
    return b;
  };

  const model = new THREE.Group();
  const head = new THREE.Group(); // carapace, eyes, chelicerae, palps
  head.position.copy(PIVOT);
  model.add(head);
  const local = (v) => v.clone().sub(PIVOT); // model frame -> head frame

  // Cephalothorax: the carapace, with the eyes placed on its surface.
  const carapace = new THREE.Mesh(carapaceGeometry(hi ? 40 : 16, hi ? 28 : 12), mats.body);
  carapace.position.copy(local(CEPH));
  head.add(carapace);
  const probe = new THREE.Mesh(carapace.geometry); // unparented: its world frame is the carapace's own
  const onCarapace = (dir, inset) => {
    const s = surface(probe, dir);
    return { p: s.p.addScaledVector(s.n, -inset).add(carapace.position), n: s.n };
  };
  const eye = new THREE.SphereGeometry(1, hi ? 20 : 8, hi ? 14 : 6);
  // Two large front eyes (AME), the smaller front-laterals (ALE), and the
  // posterior medians (PME) and laterals (PLE) on top.
  const EYES = [[0.18, 1, 0.25, 0.21], [0.45, 0.85, 0.45, 0.11], [0.5, 0.5, 0.75, 0.045], [0.55, 0.05, 0.8, 0.095]];
  for (const [x, y, z, r] of EYES) {
    for (const side of [-1, 1]) {
      const at = onCarapace(new THREE.Vector3(side * x, y, z), r * 0.35);
      const e = new THREE.Mesh(eye, mats.eye);
      e.scale.setScalar(r);
      e.position.copy(at.p);
      head.add(e);
    }
  }
  if (hi) {
    const pos = carapace.geometry.attributes.position;
    const nor = carapace.geometry.attributes.normal;
    const dorsal = [];
    for (let i = 0; i < pos.count; i++) if (pos.getZ(i) > 0.15 && pos.getY(i) < 0.9) dorsal.push(i);
    hairs(carapace, 120, () => {
      const i = dorsal[Math.floor(rand() * dorsal.length)];
      const p = new THREE.Vector3(pos.getX(i), pos.getY(i), pos.getZ(i));
      const d = new THREE.Vector3(nor.getX(i), nor.getY(i), nor.getZ(i)).add(new THREE.Vector3(0, -0.8, 0));
      return { p, d, len: 0.6 + 0.5 * rand() };
    }, budget, hairGeo);
  }

  // Chelicerae with fangs, below the front eyes.
  for (const side of [-1, 1]) {
    const at = onCarapace(new THREE.Vector3(side * 0.2, 1, -0.45), 0.08);
    const g = new THREE.Group();
    g.position.copy(at.p);
    g.lookAt(g.position.clone().add(new THREE.Vector3(side * 0.05, 0.35, -1))); // +z toward the tip
    g.rotateY(-Math.PI / 2); // +x toward the tip
    g.add(new THREE.Mesh(taper(0.55, 0.16, 0.1, radial), mats.body));
    const fang = new THREE.Mesh(new THREE.ConeGeometry(0.06, 0.2, radial), mats.joint);
    fang.position.set(0.6, 0, 0);
    fang.rotation.z = -Math.PI / 2 - side * 0.6;
    g.add(fang);
    head.add(g);
  }

  // Pedipalps: three short segments either side of the chelicerae.
  const palps = [];
  for (const side of [-1, 1]) {
    const at = onCarapace(new THREE.Vector3(side * 0.55, 0.85, -0.35), 0.05);
    const base = new THREE.Group();
    base.position.copy(at.p);
    base.rotation.z = side > 0 ? 1.22 : Math.PI - 1.22; // forward and out
    const j1 = new THREE.Group();
    j1.rotation.y = -0.4;
    base.add(j1);
    j1.add(new THREE.Mesh(taper(0.4, 0.08, 0.07, radial), mats.body));
    ball(j1, 0.08);
    const j2 = new THREE.Group();
    j2.position.x = 0.4;
    j2.rotation.y = 1.0;
    j1.add(j2);
    j2.add(new THREE.Mesh(taper(0.3, 0.07, 0.065, radial), mats.body));
    ball(j2, 0.07);
    const j3 = new THREE.Group();
    j3.position.x = 0.3;
    j3.rotation.y = 0.5;
    j2.add(j3);
    j3.add(new THREE.Mesh(taper(0.34, 0.075, 0.05, radial), mats.body));
    ball(j3, 0.072);
    head.add(base);
    palps.push({ j2, j3 });
  }

  // Pedicel and abdomen (with spinnerets).
  const pedicel = new THREE.Mesh(new THREE.CylinderGeometry(0.16, 0.16, 0.35, radial), mats.joint);
  pedicel.position.set(0, -0.75, 0.1);
  model.add(pedicel);
  const abdomen = new THREE.Mesh(abdomenGeometry(hi ? 36 : 14), mats.body);
  abdomen.position.set(0, -0.85 - 1.54 * ABDOMEN, 0.18);
  model.add(abdomen);
  for (const side of [-1, 1]) {
    const sp = new THREE.Mesh(new THREE.ConeGeometry(0.07, 0.22, radial), mats.joint);
    sp.position.set(side * 0.07, -1.6 * ABDOMEN, 0);
    sp.rotation.x = Math.PI;
    abdomen.add(sp);
  }
  if (hi) {
    const pos = abdomen.geometry.attributes.position;
    const nor = abdomen.geometry.attributes.normal;
    hairs(abdomen, 800, () => {
      const i = Math.floor(rand() * pos.count);
      const p = new THREE.Vector3(pos.getX(i), pos.getY(i), pos.getZ(i));
      const d = new THREE.Vector3(nor.getX(i), nor.getY(i), nor.getZ(i)).add(new THREE.Vector3(0, -0.9, 0));
      return { p, d, len: 0.6 + 0.6 * rand() };
    }, budget, hairGeo);
  }

  // Legs: coxa, then femur, patella+tibia, metatarsus, tarsus.
  const legs = [];
  LEGS.forEach((L, i) => {
    for (const side of [1, -1]) {
      const base = new THREE.Group();
      base.position.set(side * 0.72, CEPH.y + L.y, -0.05);
      ball(base, 0.15); // coxa
      const joints = [];
      let parent = base;
      SEG.forEach((share, s) => {
        const len = L.len * share;
        const j = new THREE.Group();
        if (s > 0) j.position.x = L.len * SEG[s - 1];
        parent.add(j);
        const r0 = SEG_R[s];
        j.add(new THREE.Mesh(taper(len, r0, r0 * 0.78, radial), mats.body));
        ball(j, r0 * 1.12);
        if (hi) {
          const n = [34, 34, 24, 12][s];
          hairs(j, n, () => {
            const t = 0.08 + 0.88 * rand();
            const a = rand() * Math.PI * 2;
            const r = lerp(r0, r0 * 0.78, t);
            const radialDir = new THREE.Vector3(0, Math.cos(a), Math.sin(a));
            return {
              p: new THREE.Vector3(t * len, radialDir.y * r, radialDir.z * r),
              d: radialDir.multiplyScalar(0.55).add(new THREE.Vector3(1, 0, 0)), // toward the tip
              len: 0.5 + 0.4 * rand(),
            };
          }, budget, hairGeo);
        }
        joints.push(j);
        parent = j;
      });
      model.add(base);
      legs.push({ base, joints, side, L, dir: THREE.MathUtils.degToRad(L.dir), gait: (i + (side > 0 ? 0 : 1)) % 2 });
    }
  });

  const sp = { model, head, abdomen, legs, palps, spinneret: new THREE.Vector3(0, -0.85 - 3.2 * ABDOMEN, 0.18), hairs: (hi ? MAX_HAIRS : 0) - budget.left };
  poseSpider(sp, restState());
  return sp;
}

function restState() {
  return { fold: 0, walk: 0, walkAmp: 0, twitch: 0, twitchLeg: null, palp: 0, palpSide: 0, breathe: 0, yaw: 0, pitch: 0 };
}

// poseSpider sets every joint from the pose state.
function poseSpider(sp, st) {
  for (const leg of sp.legs) {
    const walk = st.walkAmp * Math.sin(st.walk + (leg.gait ? Math.PI : 0));
    const theta = leg.dir + Math.sign(leg.dir) * (Math.PI / 2 - Math.abs(leg.dir)) * FOLD.yawIn * st.fold + 0.22 * walk;
    leg.base.rotation.z = leg.side > 0 ? theta : Math.PI - theta;
    const [femur, tibia, meta, tarsus] = leg.joints;
    femur.rotation.y = -(lerp(leg.L.lift, FOLD.lift, st.fold) + 0.35 * Math.max(0, walk));
    tibia.rotation.y = lerp(leg.L.knee, FOLD.knee, st.fold) - 0.25 * walk + (st.twitchLeg === leg ? st.twitch : 0);
    meta.rotation.y = lerp(META, FOLD.meta, st.fold);
    tarsus.rotation.y = lerp(TARSUS, FOLD.tarsus, st.fold);
  }
  sp.palps.forEach((p, i) => {
    const tw = st.palpSide === i ? st.palp : 0;
    p.j2.rotation.y = 1.0 - tw;
    p.j3.rotation.y = 0.5 + 0.6 * tw;
  });
  const b = 1 + st.breathe;
  sp.abdomen.scale.set(b, 1 + st.breathe * 0.4, b);
  sp.head.rotation.set(st.pitch, 0, st.yaw);
}

// --- render loop -----------------------------------------------------------

function request(minGap) {
  if (!renderer || document.hidden || calm() || raf || timer) return; // Calm hides the layer
  const wait = Math.max(0, (minGap || ACTIVE_MS) - (performance.now() - lastFrame));
  timer = setTimeout(() => {
    timer = 0;
    raf = requestAnimationFrame(tick);
  }, wait);
}

function tick(now) {
  raf = 0;
  const dt = lastFrame ? Math.min(now - lastFrame, 250) : 16;
  lastFrame = now;
  const t0 = performance.now();
  const kind = anims.size ? "active" : "idle";
  for (const a of anims) {
    const t = Math.min(1, (now - a.start) / a.dur);
    a.step(t);
    if (t >= 1) {
      anims.delete(a);
      a.resolve();
    }
  }
  const alive = idleOn();
  if (alive) heroView.live(now, dt);
  const easing = easeTilt(dt);
  draw();
  frames++;
  const s = stats[kind];
  s.n++;
  s.ms += performance.now() - t0;
  if (anims.size) request(ACTIVE_MS);
  else if (alive || easing) request(IDLE_MS);
}

// animate runs step(t), t from 0 to 1, over dur ms; when the 3D toggle is
// off it jumps to the end at once.
function animate(dur, step) {
  if (!spiderOn() || document.hidden) {
    step(1);
    return Promise.resolve();
  }
  return new Promise((resolve) => {
    anims.add({ start: performance.now(), dur, step, resolve });
    request(0);
  });
}

function finishAll() {
  for (const a of anims) {
    a.step(1);
    a.resolve();
  }
  anims.clear();
}

function draw() {
  const w = root.clientWidth;
  const h = root.clientHeight;
  renderer.setScissorTest(false);
  renderer.clear();
  renderer.setScissorTest(true);
  for (const v of [heroView, toggleView]) {
    if (!v.active()) continue;
    const r = v.rect();
    if (!r || r.width <= 0 || r.height <= 0 || r.bottom < 0 || r.top > h || r.right < 0 || r.left > w) continue;
    v.update(r);
    const y = h - r.bottom;
    renderer.setViewport(r.left, y, r.width, r.height);
    renderer.setScissor(r.left, y, r.width, r.height);
    renderer.render(v.scene, v.camera);
  }
}

// fitCamera makes the plane z = 0 map 1 unit to 1 CSS px over w x h.
function fitCamera(camera, w, h) {
  camera.aspect = w / h;
  const z = h / 2 / Math.tan(THREE.MathUtils.degToRad(camera.fov / 2));
  camera.position.set(0, 0, z);
  camera.near = Math.max(1, z - 400);
  camera.far = z + 400;
  camera.updateProjectionMatrix();
}

// placed puts the model in a group turned by orient and centred on its own
// bounding box (x and y; z from its lowest point, so standing legs rest on
// z = 0); it returns that group and the model's height in model units.
function placed(sp, orient) {
  const o = new THREE.Group();
  orient(o);
  o.add(sp.model);
  o.updateMatrixWorld(true);
  const box = new THREE.Box3().setFromObject(o);
  const c = box.getCenter(new THREE.Vector3());
  o.position.set(-c.x, -c.y, -box.min.z);
  const g = new THREE.Group();
  g.add(o);
  return { group: g, height: box.max.y - box.min.y, depth: box.max.z - box.min.z };
}

// --- toggle view: the spider hanging on its thread -------------------------

function makeToggleView() {
  const btn = document.getElementById("theme-toggle");
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(22, 1, 1, 2000);
  lights(scene);
  const sp = buildSpider("low");
  const hang = placed(sp, (o) => o.rotation.set(0.35, 0, Math.PI)); // head down, back to the viewer
  hang.group.children[0].position.z -= hang.depth / 2; // centre in depth too
  const rig = new THREE.Group(); // drop and twist
  rig.scale.setScalar(TOGGLE_PX / hang.height);
  rig.add(hang.group);
  scene.add(rig);
  const threadGeo = new THREE.BufferGeometry().setFromPoints([new THREE.Vector3(), new THREE.Vector3()]);
  scene.add(new THREE.Line(threadGeo, mats.thread));
  const st = Object.assign(restState(), { drop: 0, twist: 0 });
  const tip = new THREE.Vector3();
  let W = 0;
  let H = 0;
  return {
    scene, camera, sp, st,
    active: spiderOn,
    rect() {
      const b = btn.getBoundingClientRect();
      const height = b.bottom + 48;
      return { left: b.left + b.width / 2 - 48, top: 0, width: 96, height, right: b.left + b.width / 2 + 48, bottom: height };
    },
    update(r) {
      if (r.width !== W || r.height !== H) {
        W = r.width;
        H = r.height;
        fitCamera(camera, W, H);
      }
      const b = btn.getBoundingClientRect();
      rig.position.set(0, H / 2 - (b.top + b.height / 2) - st.drop * DROP_PX, 0);
      rig.rotation.y = st.twist;
      poseSpider(sp, st);
      rig.updateMatrixWorld(true);
      sp.model.localToWorld(tip.copy(sp.spinneret));
      const pos = threadGeo.attributes.position;
      pos.setXYZ(0, tip.x, H / 2, 0);
      pos.setXYZ(1, tip.x, tip.y, tip.z);
      pos.needsUpdate = true;
    },
  };
}

// --- hero view: the detailed spider on its orb web -------------------------

// orbWeb returns an orb web of radius 1 as segments in three opacity bands
// (hub, middle, rim): spokes plus a spiral that sags toward the hub.
function orbWeb(seed) {
  const rand = rng(seed);
  const n = 16;
  const rings = 11;
  const spokes = [];
  for (let i = 0; i < n; i++) {
    const a = (i / n) * 2 * Math.PI + (rand() - 0.5) * 0.15;
    spokes.push({ dx: Math.cos(a), dy: Math.sin(a), len: 0.92 + 0.08 * rand() });
  }
  const bands = [[], [], []];
  const band = (r) => (r < 0.35 ? 0 : r < 0.7 ? 1 : 2);
  const cuts = [0, 0.35, 0.7];
  for (const s of spokes) {
    for (let b = 0; b < 3; b++) {
      const from = cuts[b];
      const to = b < 2 ? Math.min(cuts[b + 1], s.len) : s.len;
      if (to > from) bands[b].push(s.dx * from, s.dy * from, 0, s.dx * to, s.dy * to, 0);
    }
  }
  for (let k = 1; k <= rings; k++) {
    const base = 0.08 + 0.9 * Math.pow(k / rings, 1.15);
    const r = spokes.map(() => base * (1 + (rand() - 0.5) * 0.04));
    const out = bands[band(base)];
    for (let i = 0; i < n; i++) {
      const j = (i + 1) % n;
      const ax = spokes[i].dx * r[i];
      const ay = spokes[i].dy * r[i];
      const bx = spokes[j].dx * r[j];
      const by = spokes[j].dy * r[j];
      const cx = ((ax + bx) / 2) * 0.88;
      const cy = ((ay + by) / 2) * 0.88;
      let px = ax;
      let py = ay;
      for (let q = 1; q <= 3; q++) {
        const t = q / 3;
        const x = (1 - t) ** 2 * ax + 2 * t * (1 - t) * cx + t * t * bx;
        const y = (1 - t) ** 2 * ay + 2 * t * (1 - t) * cy + t * t * by;
        out.push(px, py, 0, x, y, 0);
        px = x;
        py = y;
      }
    }
  }
  return bands.map((v) => new Float32Array(v));
}

function makeHeroView() {
  const slot = document.getElementById("hero-web");
  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(30, 1, 1, 2000);
  lights(scene);
  const stage = new THREE.Group(); // web and spider, tilted toward the viewer
  const BASE = { x: 0.22, y: -0.2, z: 0 }; // a mostly top-down view, tilted a little
  scene.add(stage);
  const web = new THREE.Group();
  const webMats = orbWeb(61).map((v) => {
    const geo = new THREE.BufferGeometry();
    geo.setAttribute("position", new THREE.BufferAttribute(v, 3));
    const mat = new THREE.LineBasicMaterial({ transparent: true, depthWrite: false });
    web.add(new THREE.LineSegments(geo, mat));
    return mat;
  });
  stage.add(web);
  const sp = buildSpider("high");
  // On the web, head up and a little turned, centred on the hub; the tarsi
  // rest on the web's plane.
  const stand = placed(sp, (o) => o.rotation.set(0, 0, 0.2));
  const body = new THREE.Group();
  body.scale.setScalar(HERO_PX / stand.height);
  body.add(stand.group);
  body.position.y = 4; // the heavy abdomen hangs low: lift the whole spider a little
  stage.add(body);
  const st = restState();
  const look = { yaw: 0, pitch: 0 };
  let palpNext = 0;
  let W = 0;
  let H = 0;

  function colors() {
    const rgb = css("--web-rgb").split(",").map((x) => Number(x) / 255);
    const hi = parseFloat(css("--web-alpha-max"));
    const lo = parseFloat(css("--web-alpha-min"));
    webMats.forEach((m, i) => {
      m.color.setRGB(rgb[0], rgb[1], rgb[2], THREE.SRGBColorSpace);
      m.opacity = lerp(hi, lo, i / 2);
    });
  }
  colors();

  return {
    scene, camera, sp, st, colors,
    active: () => !!slot && slot.offsetParent !== null,
    rect() {
      const r = slot.getBoundingClientRect();
      return { left: r.left - 10, top: r.top - HERO_OVERFLOW, width: r.width + 20, height: r.height + 2 * HERO_OVERFLOW,
        right: r.right + 10, bottom: r.bottom + HERO_OVERFLOW };
    },
    // live advances the idle life: breathing, a pedipalp twitch now and
    // then, and the head easing toward the pointer.
    live(now, dt) {
      st.breathe = 0.025 * Math.sin((2 * Math.PI * now) / 3200);
      if (now > palpNext) {
        palpNext = now + 2500 + Math.random() * 4000;
        st.palpSide = Math.random() < 0.5 ? 0 : 1;
        st.palpStart = now;
      }
      const pt = st.palpStart ? (now - st.palpStart) / 220 : 1;
      st.palp = pt < 1 ? 0.5 * Math.sin(Math.PI * pt) : 0;
      const r = slot.getBoundingClientRect();
      if (pointer.x >= 0) {
        look.yaw = clamp((pointer.x - (r.left + r.width / 2)) / 250, -1, 1) * 0.45;
        look.pitch = clamp((pointer.y - (r.top + r.height / 2)) / 250, -1, 1) * 0.25;
      }
      const k = 1 - Math.exp(-dt / 150);
      st.yaw += (-look.yaw - st.yaw) * k; // the model's z axis points out of the web
      st.pitch += (look.pitch - st.pitch) * k;
    },
    update(r) {
      if (r.width !== W || r.height !== H) {
        W = r.width;
        H = r.height;
        fitCamera(camera, W, H);
        // An oval web (a round one seen at an angle) that stays inside the
        // hero card; the spider may step over the card's edge.
        web.scale.set(Math.min(W, 190) * 0.47, (H - 2 * HERO_OVERFLOW) * 0.62, 1);
      }
      stage.rotation.set(BASE.x + tilt.x, BASE.y + tilt.y, BASE.z);
      poseSpider(sp, st);
    },
  };
}

// easeTilt moves the hero toward the pointer's tilt; true while moving.
function easeTilt(dt) {
  const dx = tilt.tx - tilt.x;
  const dy = tilt.ty - tilt.y;
  if (Math.abs(dx) < 0.0005 && Math.abs(dy) < 0.0005) {
    tilt.x = tilt.tx;
    tilt.y = tilt.ty;
    return false;
  }
  const k = 1 - Math.exp(-dt / 120);
  tilt.x += dx * k;
  tilt.y += dy * k;
  return true;
}

// --- wiring ------------------------------------------------------------------

function resize() {
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, MAX_DPR));
  // The layer covers the viewport without its scrollbar (CSS 100%), so
  // size the drawing buffer to the same box.
  renderer.setSize(root.clientWidth, root.clientHeight, false);
  request(0);
}

// sync applies Calm and reduced motion: the class swaps the SVG spiders for
// the 3D ones; a running animation jumps to its end.
function sync() {
  const on = spiderOn();
  root.classList.toggle("spider-3d", on);
  if (!on) {
    finishAll();
    tilt.tx = 0;
    tilt.ty = 0;
  }
  scheduleTwitch();
  request(0);
}

// scheduleTwitch plans the toggle spider's next leg twitch, 7 to 13 s away.
function scheduleTwitch() {
  clearTimeout(twitchTimer);
  if (!spiderOn() || !idleTwitch) return;
  twitchTimer = setTimeout(() => {
    const st = toggleView.st;
    if (spiderOn() && !busy && !document.hidden) {
      st.twitchLeg = toggleView.sp.legs[Math.floor(Math.random() * toggleView.sp.legs.length)];
      animate(280, (t) => {
        st.twitch = 0.35 * Math.sin(Math.PI * t);
      }).then(() => {
        st.twitchLeg = null;
      });
    }
    scheduleTwitch();
  }, 7000 + Math.random() * 6000);
}

// toggle plays the theme switch on the 3D spider: it drops on its thread
// with a slight twist while its legs fold in and its colour blends to the
// next theme's; atBottom() switches the theme; then it climbs back with an
// alternating four-leg gait.
async function toggle(next, atBottom) {
  const st = toggleView.st;
  const from = spiderColor(TS.theme.current());
  const to = spiderColor(next);
  busy = true;
  await animate(DROP_MS, (t) => {
    const e = easeInOut(t);
    st.drop = e;
    st.twist = 0.5 * e;
    st.fold = e;
    paint(from, to, e);
  });
  atBottom();
  await animate(HOLD_MS, () => {});
  await animate(CLIMB_MS, (t) => {
    const e = easeOut(t);
    st.drop = 1 - e;
    st.twist = 0.5 * (1 - e);
    st.fold = Math.max(0, 1 - t * 2.2);
    st.walk = t * Math.PI * 6;
    st.walkAmp = Math.sin(Math.PI * t);
  });
  st.walkAmp = 0;
  busy = false;
  paint(spiderColor(TS.theme.current()));
  request(0);
}

function init() {
  const canvas = document.createElement("canvas");
  canvas.className = "gl-layer";
  canvas.setAttribute("aria-hidden", "true");
  try {
    renderer = new THREE.WebGLRenderer({ canvas, alpha: true, antialias: true, powerPreference: "low-power" });
  } catch (e) {
    renderer = null; // no WebGL: the SVG spiders stay (decor.js, index.html)
    root.classList.add("no-webgl");
    return;
  }
  renderer.setClearColor(0x000000, 0);
  renderer.autoClear = false;
  document.body.appendChild(canvas);
  mats = makeMaterials();
  paint(spiderColor(TS.theme.current()));
  toggleView = makeToggleView();
  heroView = makeHeroView();
  resize();
  window.addEventListener("resize", resize);
  window.addEventListener("scroll", () => request(0), { passive: true });
  const slot = document.getElementById("hero-web");
  new IntersectionObserver((entries) => {
    heroVisible = entries.some((e) => e.isIntersecting);
    request(0);
  }).observe(slot);
  window.addEventListener("pointermove", (e) => {
    pointer.x = e.clientX;
    pointer.y = e.clientY;
    if (!idleOn()) return;
    tilt.ty = (e.clientX / window.innerWidth - 0.5) * 2 * TILT;
    tilt.tx = (e.clientY / window.innerHeight - 0.5) * 2 * TILT;
  }, { passive: true });
  document.addEventListener("ts:theme", () => {
    if (!busy) paint(spiderColor(TS.theme.current()));
    mats.thread.color.set(css("--ink"));
    heroView.colors();
    request(0);
  });
  mats.thread.color.set(css("--ink"));
  document.addEventListener("ts:calm", sync);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) {
      clearTimeout(timer);
      timer = 0;
      if (raf) cancelAnimationFrame(raf);
      raf = 0;
      clearTimeout(twitchTimer);
    } else {
      scheduleTwitch();
      request(0);
    }
  });
  if (reduce.addEventListener) reduce.addEventListener("change", sync);
  sync();
}

TS.Spider3D = {
  ready: () => spiderOn() && !busy,
  toggle,
  // For checks (check-theme.ps1): frames drawn, pixel ratio, animating,
  // frame timing, the hair count, and switching the toggle spider's random
  // idle twitch off so that measured windows stay predictable.
  frames: () => frames,
  pixelRatio: () => (renderer ? renderer.getPixelRatio() : 0),
  animating: () => anims.size > 0,
  stats: () => ({
    activeFrames: stats.active.n, activeMs: stats.active.n ? stats.active.ms / stats.active.n : 0,
    idleFrames: stats.idle.n, idleMs: stats.idle.n ? stats.idle.ms / stats.idle.n : 0,
  }),
  hairs: () => (heroView ? heroView.sp.hairs : 0),
  setIdleTwitch(on) {
    idleTwitch = !!on;
    scheduleTwitch();
  },
};

init();
