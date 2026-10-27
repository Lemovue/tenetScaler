# Harshit: Gateway and Explainability (the fast loop)

_Overall status: `00_PROJECT_STATUS.md`. The old script is `TenantScaler_Final/05_Harshit.pdf`; where the two differ, this file is newer._

## Tumhara role

Gateway har request ko handle karta hai: kaun pehle jaayega, kaun 429 paayega, aur controller ka faisla request level pe kaise lagu hota hai. Saath mein, the decision log and dashboard explain every decision.

## Code kahan hai

| What | File |
|---|---|
| Admission (shed or queue) | `internal/fairq/admit.go` |
| Who goes next (fair counters, slot reservation) | `internal/fairq/dispatch.go` |
| HTTP gateway (`/v1/completions`, `/policy`, `/stats`, `/metrics`) | `internal/gateway/gateway.go`, `completions.go` |
| Proxy to llama-server (streaming, keep-alive retry fix) | `internal/gateway/proxy.go` |
| API keys (key per team, admin key) | `internal/gateway/auth.go` |
| Decision log on the dashboard | `internal/web/static/decisions.js` |

Run your tests: `go test ./internal/fairq/ ./internal/gateway/` (all pass).

## Ek request ka safar (whiteboard pe banao)

1. **Request arrives** with the team's API key (or the `x-llm-d-inference-fairness-id` header, which is compatible with llm-d) and an optional priority: critical / standard / sheddable.
2. **Counted as offered demand** straight away, even if it gets shed later. The controller needs the true load.
3. **Admission:**
   - **Critical quota:** a team gets at most "guarantee × 4" critical requests. Above that they count as standard, so nobody can cheat by marking everything critical.
   - **Over-share:** if the controller marked this team over its share, **and** the pool is busy, its sheddable requests get **429 + Retry-After**. Retry-After is jittered so clients don't all retry together. If slots are free, the request is served (we don't waste capacity).
   - **Deadline:** a sheddable request that can't start within its TTFT target is rejected early.
4. **Queue → dispatch** when a replica slot is free. At most 4 requests are in flight per replica (the dispatch window).
   - The **next team** is the one with the lowest fair counter, where counter += tokens ÷ weight (this is VTC, from OSDI '24).
   - **Inside a team:** critical first, then earliest deadline.
   - **Slot reservation:** a team that is over its share can't take the slots that another active team's share still needs. Requests can't be paused once running, so this is how a guarantee actually holds.
5. **Proxy to llama-server**, stream the tokens back, and charge the counter with the real token count.

## Real bugs we found and fixed (Q&A stories)

| Problem | Fix | Evidence |
|---|---|---|
| We shed 41.5% of C's traffic even when slots were free | Shed only while the pool is busy | SIMULATED: 41.5% → 0.2%, victims still 0% |
| 2 of 223 requests failed with 502 on real replicas | llama-server closes idle connections after 5 s; the gateway now resends such a request safely | Regression test reproduces the exact `EOF` → 502 |
| Victims waited behind C's borrowed slots | Slot reservation for over-share teams | Victim violation 10.9% → 5.9% (SIMULATED, calibrated to real) |

## Sawaal: English jawab

_These answers describe the finished system. Anything still marked ⬜ in `00_PROJECT_STATUS.md` must be done before you say it on stage._

| Sawaal | Jawab |
|---|---|
| "What is VTC?" | "A virtual token counter per tenant. Each served request adds its tokens divided by the tenant's weight, and the tenant with the lowest counter goes next, so service is fair in tokens, not in request counts." |
| "Why 429 instead of just queuing?" | "Queuing an over-share team's batch traffic would delay everyone behind it. We reject only its sheddable requests, only while the pool is busy, and tell the client when to retry." |
| "Can a team cheat by marking everything critical?" | "No. Critical requests are capped at the team's guaranteed slots; above that they are treated as standard. With API keys, the key will also cap the highest priority a team may use (being built, N5)." |
| "What if the controller dies?" | "The gateway keeps the last policy it received, and the controller fails static." |
| "How fast does a new decision reach the gateway?" | "The controller pushes a versioned policy over HTTP every cycle. Propagation is measured; it was well under a millisecond locally." |

## Tumhare liye kya baaki hai (phase-wise)

| Phase | Task | Milestone |
|---|---|---|
| 2 | API key sets the team **and its maximum priority**; a client header may only *lower* priority. Gateway can listen on 0.0.0.0 for teammates' laptops. | N5 |
| 3 | Count tokens with llama-server's own tokenizer instead of counting words | N4 |
| 4 | Dashboard: label live traffic "live demo, not the experiment"; fix the open review items (results tab path, error banners, chart colour contrast) | N5 |
| 4 | Demo: be ready to point at the dashed SLO line and the 429 marks on screen | rehearsal |
