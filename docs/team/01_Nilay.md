# Nilay: Pitch, story and demo narration

_Overall status: `00_PROJECT_STATUS.md`. The old script is `TenantScaler_Final/03_Nilay.pdf`; where the two differ, this file is newer._

## Tumhara role

Presentation tum shuru aur khatam karoge, demo narrate karoge, aur "big picture" sawaalon ka jawab doge. Code likhna tumhara kaam nahi hai. Lekin **har number ka source** tumhe pata hona chahiye, aur ye bhi ki woh REAL hai ya SIMULATED.

## Story, 4 lines mein (English, judges ke liye)

1. "When many teams share one LLM on a fixed budget of replicas, one team's spike makes everyone else slow, even though their own traffic did not change."
2. "Autoscalers like HPA and KEDA decide **how many** replicas to run. They cannot decide **who** gets them, and new replicas take time to start."
3. "TenantScaler gives every team a guaranteed share, lends idle shares to busy teams and takes them back when the owner returns. The gateway enforces the same decision on every request within one control cycle."
4. "We measured this on real llama.cpp replicas, against KEDA and against a Flow-Control-style baseline, with the same traffic, the same budget, repeated runs and confidence intervals."

## Ab tak kya bana hai (jo tum dikhaoge)

- **Dashboard** (`./scripts/run-demo.ps1`, then open http://127.0.0.1:8080):
  - **Run A/B** plays the same recorded traffic first on the KEDA baseline, then on TenantScaler.
  - Both use 4 real llama.cpp replicas.
  - On real replicas a full run takes **about 5–6 minutes**, so start it before you begin talking.
- **Decision log** (dashboard ke neeche): har 10 second ka faisla, plain English mein. Example, from a real run: *"C got 2 replicas: guarantee 1 plus 1 borrowed… its sheddable requests get 429."*
- **Results tab:** real and simulated summaries, each labelled.

## Numbers tum bol sakte ho (abhi ke, early)

| Line | Number | Kya hai |
|---|---|---|
| "In one real run, victims missing their SLO dropped from 91% to 4%." | 91.3% → 4.3% | REAL, **n = 1**: always say "in one run" |
| "Team A's slowest answers went from 44 seconds to under 2 seconds." | p99 TTFT 44.0 s → 1.8 s | REAL, n = 1 |
| "Four replicas, one per CPU core, serve about 467 weighted tokens per second together." | 466.7 | REAL, measured |

**Abhi mat bolna:** "5 runs", "95% confidence" or "S7 beats S4". These come only after the N6 runs (see `00_PROJECT_STATUS.md`).

## Pre-registered claim (tumhe yaad hona chahiye)

This goes into the README **before** the real runs, and we report the result whichever way it goes:

> "During C's spike, at equal replica budget, S7 has higher victim (A, B) SLO attainment (TTFT p99 and TPOT both met) than S4, with C's shed % and shed-aware attainment reported alongside."

S7 = TenantScaler. S4 = a baseline that works like llm-d Flow Control. **This is our novelty test.** If S7 doesn't win, we say so honestly. Judges respect that more than a hidden result.

## Tough sawaal: English jawab

_These answers describe the finished system. Anything still marked ⬜ in `00_PROJECT_STATUS.md` must be done before you say it on stage._

| Sawaal | Jawab |
|---|---|
| "Isn't this just llm-d Flow Control?" | "llm-d Flow Control already does per-tenant queues, priority and 429 shedding, and we build on that design. What it lacks is guaranteed shares that move with the autoscaler: its round-robin gives every tenant an equal turn. Our S4 baseline re-implements that design, and S7 vs S4 is our pre-registered test." |
| "Why CPU, not GPU?" | "We measured it. On one shared laptop GPU, 1 to 6 replicas gave the same total throughput, about 630 tokens per second, so extra replicas added nothing. CPU replicas pinned to separate cores give a real, isolated budget. A GPU fleet changes the absolute numbers, not the comparison." |
| "What's the downside?" | "The noisy team is throttled while it is over its share. In simulation with a tight budget, a simpler baseline actually did slightly better for the victims. We report that too." |
| "Is it real or simulated?" | "Headline numbers come from real llama.cpp inference. The simulator only extends to setups we can't run on a laptop, and every simulated number is labelled." |
| "How did you compare fairly?" | "Same recorded traffic and seed, same model, same budget, setups run in a random interleaved order, at least 5 runs each, mean with 95% confidence intervals and a drift check." |

## Tumhare liye kya baaki hai (phase-wise)

| Phase | Task | When |
|---|---|---|
| 3 | Approve the pre-registered claim wording before the real runs start | before N6 |
| 4 | Practise DEMO.md (phone hotspot, laptop plugged in, max-performance power plan) | after N5 |
| 4 | Live demo: teammates' laptops send traffic with `tenantscaler loadgen`. You narrate "live demo, not the experiment" | after N5 |
| 4 | Update the slides only with numbers from `results/real/summary.md` | after N6 |
| 4 | Record the backup video with Akshat; run two full mocks | end |

Note: the Kaggle/GPU plan has been cut (decision Q9), so there are no tunnels and the demo runs on the laptop.
