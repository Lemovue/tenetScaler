# Spider-web theme: notes

## Mood taken from the reference (eranamjoshi.com, via Site of Sites)

1. Calm, generous empty space, with a few hand-made marks giving all the personality; the content itself stays plain.
2. Flat colour fields and thin lines: no shadows, no gradients; depth comes from type weight and line, not effects.
3. Lowercase, conversational labels; one large light-weight statement beside small bold labels.
4. Motion is gentle and staged, one thing at a time, never a busy loop.
5. One loud accent at a time against a quiet neutral ground; decoration lives at the edges, data stays in the centre.

Nothing is copied from the reference: no code, SVG paths, images, text, fonts or layout. The webs and the spider are drawn from scratch (`static/decor.js`, `static/index.html`), and the fonts are the system font stack.

## Style taken from the comic-print reference (the hackathon site, style only)

1. Comic lettering for headings over a plain, quiet body face.
2. Print texture: a fine halftone dot screen in headers, never over data.
3. Ink panels: thick black outlines with a hard, unblurred offset shadow, as if cut out and pasted down.
4. Off-register printing: a thin red and cyan fringe beside headline letters, still, not animated.
5. Narration as caption boxes, and short, sharp scene cuts instead of slow fades.

Nothing is taken from that site but these style ideas: no images, logos, characters, names, numbers, text or code. Nothing in the UI depicts or names any film or comic character.

## What changed, round by round

1. **Spider-web theme.** Dark (default) and light themes; procedural corner webs; a spider theme toggle; Calm; MET / MISSED / SHED labels with icons; SIMULATED / REAL / INJECTED badges; every text on a plate at least 86% opaque.
2. **3D spider, themed motion, polish.** "TenantScaler" and acronyms in caps, sentence case elsewhere (no lowercase transform); three.js r170 vendored; webs spun in on first load, then still; the webs on each panel's side tremble with that stack's request rate during a run; the hero cut to one line; empty charts say "Waiting for a run: press Run A/B"; the offline banner shows the start command.
3. **Detailed original 3D spider and chart fixes.** An anatomical spider (below) on the hero web and at 40 px as the toggle, one renderer; TTFT chart: dashed "SLO" line, y = max(1.25 x SLO, data max); replicas chart: whole-number ticks 0 to the budget with a dashed "budget" line; x axis = run time, auto-ranged from the data; one consistent offline state ("Offline" pill and badges, no "Loading...").
4. **Comic-print layer (chrome only).** Bangers headings with a static red/cyan misregistration; halftone dots in the hero and section headers; ink panels (2.5 px outline, hard 4 px offset shadow); caption boxes for decision-log events; a 250 ms red/cyan glitch on view and theme switches.

## What is drawn, and how

- **Corner webs** (`static/decor.js`): three procedural orb webs (14 spokes, a 15-turn sagging spiral, seeded) anchored top-right, top-left and bottom-left on a fixed 2D layer behind the page. First load: spun strand by strand in 1.5 s, then still. During a run they tremble, 2 px per request/s of the stack on their side (baseline left, TenantScaler right; the gateway's arrival rate from `/api/state`), at most 10 px.
- **The spider** (`static/spider3d.js`, three.js r170): an original model after real jumping spiders and orb-weavers, built from primitives: cephalothorax with a raised carapace; eight eyes, two large glossy front eyes, placed on the carapace by raycasting; chelicerae with fangs; pedipalps; pedicel; a lathe-turned tapered abdomen with spinnerets; eight legs of four visible segments (femur, patella+tibia, metatarsus, tarsus), tapered cylinders with joint spheres, knees arching above the body. A runtime value-noise bump map; 1,752 hair instances (at most 2,000) on legs, carapace and abdomen; MeshPhysicalMaterial with clearcoat and sheen; key, fill and a strong rim light. One theme hue (blue dark, red light) and darker shades of it at the joints, fangs and eyes; no stripes, patterns, emblems or markings. Not modelled on any film or comic character.
- **Hero**: the detailed spider (80 px tall) on a tilted 3D orb web in the hero card. Idle: abdomen breathing, a pedipalp twitch every 2.5-6.5 s, the head turning toward the pointer, a slight parallax tilt.
- **Toggle**: the same model at 40 px with reduced detail (no hairs, fewer polygons) hanging on its thread; a `<button>` (44 x 44, focus ring, `aria-label` "Switch to light/dark theme"). On click or Enter/Space it drops with a twist, legs folding in and its colour blending to the next theme's; the theme switches at the bottom inside the glitch; it climbs back with an alternating four-leg gait. A rare idle leg twitch.
- **One WebGL renderer** for both views: one canvas over the page (pointer-events none, aria-hidden), each view scissored to its DOM anchor.
- **Fallbacks**: under Calm or without WebGL, the 2D SVG spider is the toggle and a 2D web with the SVG spider fills the hero slot; under reduced motion the 3D stays still.
- **Comic layer** (`static/comic.js`, `style.css`): Bangers (SIL OFL 1.1, `static/fonts/`) for h1 and h2 only; halftone in the hero and section headers only; ink panels; captions and the glitch (below). Chart plots, axes, legends and the per-tenant tables keep the plain body font, no halftone, no shadows.

## Data stays readable

- SLO status: **MET** (check) / **MISSED** (cross) / **SHED** (slashed circle, dashed) in the per-tenant tables; MET means attainment reached the tenant's `targetFraction` from `/api/config`. Status colours are at least CIEDE2000 20 away from the theme red and blue.
- Badges: **SIMULATED** black on yellow in both themes; **REAL** inverted; **INJECTED n s LOAD DELAY** (dashed) on real results with `readinessHoldSeconds` above 0.
- Captions come only from the decision log (`/api/decisions`): a tenant's demand rising above its guarantee ("Tenant C spike detected: it wants 3 replicas"), shedding starting or stopping, a tenant reclaiming its share after an idle cycle, borrowing idle replicas, fail-static. Numbers are the records' own integers (demand, guarantee, grant); nothing is estimated. One at a time, 4 s each, across the panels' top edge (in the gap and the panels' padding, over no text or chart); each is also tagged "Caption: ..." on its record in the decision log; aria-hidden (the log carries it); off in Calm.
- Contrast: every pair is in the tables below, recomputed by `contrast.mjs` from `style.css`.

## Calm, motion and performance

- **Calm** (top bar, remembered) freezes all decoration: no 3D (the WebGL layer is hidden), SVG spiders, no tremble, no captions, no glitch. It comes on by itself during runs on **real** replicas, where decoration would share the CPU with the replicas being measured, and can still be switched off for the show; simulated runs show the tremble and captions. (An earlier round turned Calm on for every run; then the data-linked tremble could never be seen, so it now applies to real replicas only.)
- **Frames on demand**: 3D at most 30 frames/s while something moves on purpose (the toggle), at most 15 for the hero's idle life, nothing when the hero is off screen (IntersectionObserver), the tab is hidden or Calm is on; device pixel ratio at most 1.5. 2D webs: frames only while spinning in or trembling, at most 30/s.
- **Frame times** from DevTools Performance traces (`FireAnimationFrame` on the renderer main thread: our render code plus WebGL submission), in headless Edge with software WebGL (SwiftShader) on 4 E-cores at low priority, so a real GPU is lighter:

| Trace | Frames/s | Main-thread time per frame, mean | p95 | GPU process tasks |
|---|---|---|---|---|
| Idle, dark (4 s) | 13.5 | 2.26 ms | 3.46 ms | 15.6 ms/s |
| Idle, light (4 s) | 13.7 | 2.06 ms | 3.19 ms | 15.5 ms/s |
| Theme toggle (2.2 s, includes idle frames) | 21.1 | 1.67 ms | 2.19 ms | 27.5 ms/s |

  The page's own counters agree: 1.43 ms per animation frame, 2.62 ms per idle frame.
- **Compare CPU before and after**: run the old dashboard (main branch, `bin\tenantscaler.exe demo`, port 8080) and this one (`demo -addr 127.0.0.1:8090`) in two browser tabs. In Edge, Shift+Esc opens the browser task manager with a CPU column per tab: read each tab idle for a minute, then during a run. Or Windows Task Manager > Processes > Microsoft Edge (expand) > the tab.

## Licences and sources

- three.js r170: `static/vendor/three.module.min.js` from the npm tarball `three-0.170.0.tgz` (registry sha512 matched), MIT, `static/vendor/LICENSE-three.txt`; sha256 08fd7545d13d2c7fb65ab691530a802dafefd638596501854f267d0fb13c39e7.
- Bangers: `static/fonts/Bangers-Regular.ttf` from github.com/google/fonts (ofl/bangers), SIL OFL 1.1, `static/fonts/OFL-Bangers.txt`; sha256 4160a7311de9342674cce9160cde9fcbb30f48190397d86ff1b70b455af65824.

## Checks

- `node internal/web/contrast.mjs` recomputes the tables below from `static/style.css` and exits 1 if any pair fails or the two light-token blocks differ.
- `./internal/web/check-theme.ps1 [-Url http://127.0.0.1:8090/] [-NoWebGL] [-Width 1920 -Height 1080]` drives headless Microsoft Edge (shipped with Windows) in a throwaway InPrivate profile (no sign-in, sync or extensions), on CPUs 12-15 at low priority. Against the running demo: with WebGL 33/33 checks pass at 1366 x 768 and at 1920 x 1080; without WebGL (the fallback path) 27/27 at both sizes.
- `scripts/lint.ps1` (gofmt, go vet, node --check) and `scripts/test.ps1` pass, including the end-to-end smoke test `test/e2e` (it builds and starts the real binary on the simulated engine, checks `/healthz`, the dashboard's files and `/api/config`, runs a full A/B on the 6 s "tiny" scenario through the dashboard's API, and stops the process by PID; a real-replica variant runs when `TS_LLAMA_SERVER` and `TS_LLAMA_MODEL` are set).
- Review screenshots, backend running (simulated engine, real-demo scenario), both themes at 1366 x 768 and 1920 x 1080: before a run, mid-run during C's spike on each stack (on the TenantScaler stack with a decision caption on screen, e.g. "Tenant C spike detected: it wants 3 replicas"), and after both stacks with the side-by-side section in view; plus close-ups of the hero and toggle spiders. 24 images, kept outside the repo.

## Contrast ratios (computed by `contrast.mjs` from `static/style.css`)

WCAG 2.x ratios; the worst background of each row counts. Text needs 4.5:1 (AA), graphics and chart lines 3:1.

### Dark theme

Page #0a0a0a; card plate rgba(10, 10, 10, 0.86) = #0a0a0a, or #1f1f1f with a web strand at full strength (0.6) behind it. A web behind a card or chart shows at 8.4% at most (limit 15%).

| Pair | On | Worst ratio | Minimum | |
|---|---|---|---|---|
| text (--ink) | page / card (no web) / card + web strand / --surface / --surface-2 | 14.59 | 4.5 | pass |
| muted text (--ink-2) | page / card (no web) / card + web strand / --surface | 7.21 | 4.5 | pass |
| inverted pills (tab, kicker, REAL, live banner) | --ink | 18.16 | 4.5 | pass |
| SIMULATED badge | --sim-bg | 14.50 | 4.5 | pass |
| INJECTED badge (dashed) | card (no web) / card + web strand | 15.19 | 4.5 | pass |
| errors | --error-bg / card (no web) / card + web strand | 11.21 | 4.5 | pass |
| command in the offline banner (code) | --surface-2 | 11.03 | 4.5 | pass |
| headings and text over the halftone (--ink) | halftone dot on card + web / halftone dot on page | 11.63 | 4.5 | pass |
| caption box text | --caption-bg | 16.53 | 4.5 | pass |
| caption box outline (graphic) | --caption-bg | 16.53 | 3 | pass |
| status MET (--met) | card (no web) / card + web strand | 9.50 | 4.5 | pass |
| status MISSED (--missed) | card (no web) / card + web strand | 7.32 | 4.5 | pass |
| status SHED (--shed) | card (no web) / card + web strand | 11.17 | 4.5 | pass |
| spider toggle (graphic) | page | 6.25 | 3 | pass |
| focus ring (graphic) | page / card (no web) / card + web strand | 15.19 | 3 | pass |
| chart line --series-1 | card (no web) / card + web strand | 15.19 | 3 | pass |
| chart line --series-2 | card (no web) / card + web strand | 8.14 | 3 | pass |
| chart line --series-3 | card (no web) / card + web strand | 9.87 | 3 | pass |
| chart line --series-4 | card (no web) / card + web strand | 10.98 | 3 | pass |
| chart line --series-5 | card (no web) / card + web strand | 8.73 | 3 | pass |
| chart line --series-6 | card (no web) / card + web strand | 8.90 | 3 | pass |
| chart line --series-7 | card (no web) / card + web strand | 7.32 | 3 | pass |
| chart line --series-8 | card (no web) / card + web strand | 11.17 | 3 | pass |
| chart line --rep-ready | card (no web) / card + web strand | 15.19 | 3 | pass |
| chart line --rep-starting | card (no web) / card + web strand | 9.87 | 3 | pass |
| chart line --threshold | card (no web) / card + web strand | 7.21 | 3 | pass |

| Distinctness (CIEDE2000) | ΔE | Minimum | |
|---|---|---|---|
| --met vs theme red #d7263d | 80.9 | 20 | pass |
| --met vs theme blue #4d8eff | 54.2 | 20 | pass |
| --missed vs theme red #d7263d | 32.1 | 20 | pass |
| --missed vs theme blue #4d8eff | 52.0 | 20 | pass |
| --shed vs theme red #d7263d | 41.4 | 20 | pass |
| --shed vs theme blue #4d8eff | 32.3 | 20 | pass |
| series 1 vs 2 (normal) | 28.1 | 15 | pass |
| series 1 vs 3 (normal) | 27.9 | 15 | pass |
| series 2 vs 3 (normal) | 55.6 | 15 | pass |
| series 1 vs 2 (protan) | 26.0 | 8 | pass |
| series 1 vs 3 (protan) | 29.0 | 8 | pass |
| series 2 vs 3 (protan) | 53.2 | 8 | pass |
| series 1 vs 2 (deutan) | 25.7 | 8 | pass |
| series 1 vs 3 (deutan) | 27.4 | 8 | pass |
| series 2 vs 3 (deutan) | 53.0 | 8 | pass |
| series 1 vs 2 (tritan) | 16.9 | 8 | pass |
| series 1 vs 3 (tritan) | 24.2 | 8 | pass |
| series 2 vs 3 (tritan) | 21.6 | 8 | pass |

### Light theme

Page #f7f7f5; card plate rgba(255, 255, 255, 0.86) = #fefefe, or #ededec with a web strand at full strength (0.5) behind it. A web behind a card or chart shows at 7.0% at most (limit 15%).

| Pair | On | Worst ratio | Minimum | |
|---|---|---|---|---|
| text (--ink) | page / card (no web) / card + web strand / --surface / --surface-2 | 15.51 | 4.5 | pass |
| muted text (--ink-2) | page / card (no web) / card + web strand / --surface | 6.34 | 4.5 | pass |
| inverted pills (tab, kicker, REAL, live banner) | --ink | 17.60 | 4.5 | pass |
| SIMULATED badge | --sim-bg | 14.50 | 4.5 | pass |
| INJECTED badge (dashed) | card (no web) / card + web strand | 16.07 | 4.5 | pass |
| errors | --error-bg / card (no web) / card + web strand | 8.51 | 4.5 | pass |
| command in the offline banner (code) | --surface-2 | 8.22 | 4.5 | pass |
| headings and text over the halftone (--ink) | halftone dot on card + web / halftone dot on page | 13.46 | 4.5 | pass |
| caption box text | --caption-bg | 16.53 | 4.5 | pass |
| caption box outline (graphic) | --caption-bg | 16.53 | 3 | pass |
| status MET (--met) | card (no web) / card + web strand | 6.07 | 4.5 | pass |
| status MISSED (--missed) | card (no web) / card + web strand | 6.01 | 4.5 | pass |
| status SHED (--shed) | card (no web) / card + web strand | 6.65 | 4.5 | pass |
| spider toggle (graphic) | page | 4.62 | 3 | pass |
| focus ring (graphic) | page / card (no web) / card + web strand | 16.07 | 3 | pass |
| chart line --series-1 | card (no web) / card + web strand | 16.07 | 3 | pass |
| chart line --series-2 | card (no web) / card + web strand | 6.05 | 3 | pass |
| chart line --series-3 | card (no web) / card + web strand | 5.04 | 3 | pass |
| chart line --series-4 | card (no web) / card + web strand | 6.02 | 3 | pass |
| chart line --series-5 | card (no web) / card + web strand | 6.71 | 3 | pass |
| chart line --series-6 | card (no web) / card + web strand | 4.66 | 3 | pass |
| chart line --series-7 | card (no web) / card + web strand | 4.41 | 3 | pass |
| chart line --series-8 | card (no web) / card + web strand | 6.65 | 3 | pass |
| chart line --rep-ready | card (no web) / card + web strand | 16.07 | 3 | pass |
| chart line --rep-starting | card (no web) / card + web strand | 5.04 | 3 | pass |
| chart line --threshold | card (no web) / card + web strand | 6.34 | 3 | pass |

| Distinctness (CIEDE2000) | ΔE | Minimum | |
|---|---|---|---|
| --met vs theme red #d7263d | 65.0 | 20 | pass |
| --met vs theme blue #4d8eff | 51.6 | 20 | pass |
| --missed vs theme red #d7263d | 32.2 | 20 | pass |
| --missed vs theme blue #4d8eff | 55.5 | 20 | pass |
| --shed vs theme red #d7263d | 30.0 | 20 | pass |
| --shed vs theme blue #4d8eff | 36.1 | 20 | pass |
| series 1 vs 2 (normal) | 38.3 | 15 | pass |
| series 1 vs 3 (normal) | 36.1 | 15 | pass |
| series 2 vs 3 (normal) | 58.4 | 15 | pass |
| series 1 vs 2 (protan) | 39.0 | 8 | pass |
| series 1 vs 3 (protan) | 33.4 | 8 | pass |
| series 2 vs 3 (protan) | 60.2 | 8 | pass |
| series 1 vs 2 (deutan) | 37.7 | 8 | pass |
| series 1 vs 3 (deutan) | 36.8 | 8 | pass |
| series 2 vs 3 (deutan) | 61.5 | 8 | pass |
| series 1 vs 2 (tritan) | 28.9 | 8 | pass |
| series 1 vs 3 (tritan) | 34.7 | 8 | pass |
| series 2 vs 3 (tritan) | 30.3 | 8 | pass |

Light tokens identical in the data-theme and no-script blocks: yes
