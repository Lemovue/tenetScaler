# TenantScaler — Design and Package Contracts

This is the implementation contract every package follows. `PLAN.md` holds the
requirements; this file holds the APIs. Module path: `tenantscaler`. Go 1.27.

## 0. Conventions (all packages)

- **Shared contracts are fixed**: `internal/model`, `internal/policy`, `internal/config`,
  `internal/engine/params.go`, `internal/envfile`, `cmd/tenantscaler/main.go`. Do not edit
  them. If you need a change, describe it in your final report instead.
- **Dependencies are fixed**: stdlib + `k8s.io/{client-go,api,apimachinery}` v0.37.1,
  `github.com/prometheus/client_golang` v1.25.0, `sigs.k8s.io/yaml`. Never run `go get`
  or `go mod tidy` and never edit `go.mod`/`go.sum`.
- **Heavy steps** (go build, go test, go vet) only through the lock wrapper, and only for
  your own packages, never `./...`:
  `python scripts/dev/heavy.py --timeout 600 -- go test -count=1 ./internal/<pkg>/...`
- `gofmt -w` your files. Doc comment on every exported identifier. No dead code, no TODO
  stubs, no panics for runtime errors. Keep files under ~400 lines.
- Tests: table-driven unit tests next to the code; deterministic (fixed seeds, injected
  clocks). No test may sleep more than ~2 s total unless it is an HTTP lifecycle test.
- Time: pure logic packages take `now time.Time` parameters instead of calling
  `time.Now()`, so the discrete-event simulator can drive them in virtual time.
- Subcommands: each lives in `cmd/tenantscaler/cmd_<name>.go` and calls
  `register(name, summary, run)` from `init()`. Use `flag.NewFlagSet(name, flag.ContinueOnError)`.
- Anything simulated is labelled: HTTP header `X-TenantScaler-Simulated: true` on simulated
  replica responses; the word **simulated** in UI/report text that shows simulated numbers.

## 1. Architecture

```
tenants ─HTTP─▶ gateway ──dispatch window──▶ replica pool, one model: real llama-server replicas
                                               (1 per P-core) | labelled simulated engine
   x-llm-d-inference-fairness-id      ▲   │ GET /stats (windowed TenantObservation)
   x-priority                  POST   │   ▼
                             /policy  controller: estimate → forecast → arbiter → stabilize
                                         │            └─▶ actuator: local pool API | K8s Deployment scale
                                         └─▶ decision log (GET /decisions) ─▶ dashboard
Evaluation: real runs (internal/realrun) on llama-server replicas; the labelled simulated appendix
(internal/sim) drives the SAME fairq + control.Core + autoscale code in virtual time.
```

Ports: every server takes `-addr`. Local replicas listen on `127.0.0.1:0` (OS-assigned).

## 2. `internal/arbiter` — who gets the scarce replicas

```go
type Input struct {
    Name        string
    Demand      int     // replicas wanted this cycle, >= 0
    Guaranteed  int     // >= 0
    MaxReplicas int     // >= Guaranteed, >= 1
    Priority    int     // higher is served first when lending spare
    SLOSlack    float64 // lower = closer to violating; used after Priority
}
type Options struct{ Borrowing bool }
type Result struct {
    Name     string
    Grant    int
    Shedding bool                 // Demand > Grant
    Steps    []model.DecisionStep // rules applied, in order
    Reason   string               // one plain-English sentence for the dashboard
}
func Arbitrate(capacity int, in []Input, opt Options) ([]Result, error) // results in input order
```

Algorithm (from the problem statement):
1. Error if capacity < 0, a name is empty/duplicated, Demand/Guaranteed < 0, MaxReplicas < 1,
   MaxReplicas < Guaranteed, or sum(Guaranteed) > capacity.
2. `grant = min(Demand, Guaranteed)` → step `{rule:"guarantee"}`.
3. `spare = capacity - sum(grant)`.
4. If `Borrowing`: hungry = tenants with `min(Demand, MaxReplicas) > grant`, sorted by
   (Priority desc, SLOSlack asc, Name asc). Give spare one replica at a time, round-robin over
   the sorted list, until spare is 0 or nobody is hungry → step `{rule:"borrow"}` per replica
   (Note says the order key, e.g. "lower SLO slack than C (0.05 < 0.10)").
5. `Shedding = Demand > grant`.
6. Reason examples: "C got 4 replicas: guarantee 3 plus 1 borrowed. It wanted 8, so its
   sheddable requests get 429 until capacity frees up." / "A got 2 replicas: its full demand,
   within its guarantee of 3."

Invariants (property tests, random inputs, fixed seed): sum(grant) ≤ capacity;
`grant ≥ min(Demand, Guaranteed)`; `grant ≤ min(Demand, MaxReplicas)` when Demand ≥ Guaranteed
else `grant == Demand`; deterministic; with Borrowing, if spare remains no tenant is hungry;
without Borrowing, `grant == min(Demand, Guaranteed)`. Worked examples: capacity 10,
guarantees 3/3/3, demand 2/4/8, B slack lower → 2/4/4, C shedding; then demand 3/4/8 → 3/4/3.

## 3. `internal/estimate`

```go
// Replicas = ceil(rate*(1+headroom)/capacityPerReplica); 0 when rate <= 0.
func Replicas(weightedTokenRate, headroom, capacityPerReplica float64) int
// Slack: (SLO.TTFT - p99 TTFT)/SLO.TTFT clamped to [-1, 1]. No completed requests in the
// window: -1 if QueueLength > 0 (starving), else 1 (idle, comfortable).
func Slack(o model.TenantObservation, slo model.SLO) float64
```

## 4. `internal/stabilize` — no flapping

```go
type Config struct {
    ScaleDownWindow time.Duration // scale down only to the max desired seen in this window
    MaxStep         int           // max change per call, >= 1
    Min, Max        int
}
type Sample struct{ Time time.Time; Desired int }
type State struct{ Current int; History []Sample }
func New(cfg Config, initial int) *Stabilizer
func (s *Stabilizer) Next(now time.Time, desired int) int // up immediately (bounded by MaxStep); down to max(desired in window), bounded by MaxStep; always in [Min, Max]
func (s *Stabilizer) State() State                       // for persistence across restarts
func Restore(cfg Config, st State) *Stabilizer
```

## 5. `internal/forecast` — the ML component (Holt-Winters)

```go
type Forecaster interface {
    Observe(v float64)          // append the next actual value
    Forecast(h int) float64     // h >= 1 steps ahead, from data observed so far only
    Ready() bool
    Name() string
}
func NewHoltWinters(alpha, beta, gamma float64, season int) *HoltWinters // additive; Ready after 2 full seasons (init: level = mean(season1), trend = (mean(season2)-mean(season1))/season, seasonal = season1 - level); forecasts clamp at >= 0
func NewNaive() *Naive                     // last value
func NewSeasonalNaive(season int) *SeasonalNaive
type Metrics struct{ MAPE, SMAPE, MAE float64; N int } // MAPE skips actuals < eps
func Evaluate(series []float64, train int, f Forecaster) Metrics // observe series[:train]; then for t >= train: predict Forecast(1) BEFORE observing series[t]
type Tracker struct{...}                   // rolling one-step-ahead APEs of a live forecaster
func NewTracker(window int) *Tracker
func (t *Tracker) Record(predicted, actual float64)
func (t *Tracker) Trusted() bool           // false until window filled; false if mean APE > 1.0 (forecast off by > 2x)
```

Leakage test: a spy series proves `Evaluate` never lets a forecaster observe index t before
forecasting t.

## 6. `internal/autoscale` — baseline autoscalers (clock-agnostic)

```go
type HPAConfig struct { Target, Tolerance float64; ScaleDownWindow time.Duration; Min, Max int }
func NewHPA(cfg HPAConfig, initial int) *HPA
// value = per-pod average metric (utilization in [0,1] for CPU-HPA)
// desired = ceil(current*value/Target), unchanged if |value/Target-1| <= Tolerance;
// scale-up limit per call: max(4, current) added (HPA default behaviour);
// scale-down: max recommendation within ScaleDownWindow; clamp [Min, Max].
func (h *HPA) Step(now time.Time, current int, value float64) int

type KEDAConfig struct { TargetPerReplica, Activation, Tolerance float64; ScaleDownWindow time.Duration; Min, Max int }
func NewKEDA(cfg KEDAConfig, initial int) *KEDA
// total = total queue length; desired = ceil(total/TargetPerReplica) (AverageValue);
// if total <= Activation desired = Min; same up-limit, down-window and clamp as HPA.
func (k *KEDA) Step(now time.Time, current int, total float64) int
```
Defaults (documented in results): HPA Target 0.7, Tolerance 0.1, window 300 s;
KEDA TargetPerReplica 4, Activation 0, window 300 s; Max = pool capacity (the budget:
pods beyond it would sit Pending).

## 7. `internal/fairq` — the gateway scheduler core (not goroutine-safe; caller locks)

```go
type Mode string // "fair" | "roundrobin" | "fcfs"
type Request struct {
    ID uint64; Tenant string; Priority model.Priority
    InputTokens, MaxOutputTokens int
    Arrival, Deadline time.Time // Deadline = Arrival + tenant TTFT SLO (zero = none)
    // set by the scheduler:
    EffectivePriority model.Priority // after critical-quota downgrade
    EstOutputTokens   float64        // output tokens charged at dispatch
    Dispatched        time.Time
}
type Verdict struct { Admitted bool; RetryAfter time.Duration; Reason string } // Reason: "over-share" | "deadline-unreachable" | "saturated" | "queue-full"
type Config struct {
    Mode Mode
    MaxQueuePerTenant   int     // default 1000
    RetryJitter         float64 // RetryAfter = base*(1+RetryJitter*U[0,1)); default 0.5
    Seed                int64
    DefaultOutputTokens float64 // initial per-tenant output-length estimate; default 128
}
type TenantState struct {
    Tenant string; Queued, InFlight, CriticalActive int
    Counter, Weight, EstOutputTokens float64; Shed bool
    Admitted int64; Rejected map[string]int64
}
func New(cfg Config) *Scheduler
func (s *Scheduler) SetPolicy(p policy.Policy) bool // false (ignored) if p.Version < current
func (s *Scheduler) Policy() policy.Policy
func (s *Scheduler) Admit(r *Request, now time.Time, saturated bool) Verdict
func (s *Scheduler) NextWithin(now time.Time, free int) *Request // nil when nothing may be dispatched into one of free slots
func (s *Scheduler) Next(now time.Time) *Request    // NextWithin without a slot count (no reservation)
func (s *Scheduler) OnOutputTokens(r *Request, produced int) // tokens produced so far (monotonic)
func (s *Scheduler) Complete(r *Request, outputTokens int, now time.Time)
func (s *Scheduler) Cancel(r *Request) bool          // remove a still-queued request
func (s *Scheduler) Len() int
func (s *Scheduler) Snapshot() []TenantState         // sorted by tenant
```

**fair (TenantScaler)** — weighted VTC across tenants, priority then EDF inside a tenant:
- Admit: (1) critical quota: if `Priority.Critical()` and the tenant's CriticalActive (queued+in-flight
  critical) ≥ policy CriticalSlots (> 0) → EffectivePriority = Standard, else = Priority.
  (2) sheddable and policy.Shed **while the pool is contended** (`saturated`, or anything
  queued) → reject "over-share"; with a free slot and an empty queue it is served (shedding is
  work-conserving, §19). (3) sheddable and predicted wait
  > Deadline-now → reject "deadline-unreachable"; predicted wait = (queued+1)/dispatch rate,
  dispatch rate = tenant dispatches in the last 10 s / 10 s; skip when rate is 0.
  (4) queued ≥ MaxQueuePerTenant and not critical → "queue-full". (5) **counter lift**: if the
  tenant has nothing queued or in flight, `counter = max(counter, min counter of tenants with
  queued or in-flight work)`, or the counter of the last tenant to go idle when none is active.
  Then enqueue. RetryAfter = policy RetryAfterSeconds (min 1 s) with jitter.
- NextWithin: tenant with the smallest counter among tenants with queued work **that may take a
  slot** (ties: name); inside it the request with highest EffectivePriority, then earliest
  Deadline (zero last), then ID. **Slot reservation** (§19): a tenant the policy sheds may take
  a slot only while its in-flight requests are below its share, or when the free−1 slots left
  still cover every other active tenant's unused share; share = max(policy Slots, GuaranteedSlots)
  for a tenant with work queued or in flight, 0 otherwise. Tenants that are not shedding are
  never held back. nil with requests queued = the free slots are reserved.
  Charge `counter += (1*in + 2*est)/weight` where est = min(MaxOutputTokens, tenant estimate),
  weight = policy.WeightFor(tenant).
- OnOutputTokens: if produced exceeds the charged estimate, charge the excess (2*Δ/weight).
- Complete: correct to the actual output (may lower the counter), update the tenant estimate
  (EWMA, α = 0.2), InFlight--.
**roundrobin (llm-d Flow-Control-equivalent, re-implemented from docs)** — strict priority
bands (critical > standard > sheddable); round-robin across tenants inside a band (one request
per turn, request-count based); FIFO inside a tenant. Admit rejects sheddable with "saturated"
when `saturated` is true; queue-full as above; ignores policy weights/shed.
**fcfs** — one global FIFO by (Arrival, ID); only queue-full rejects.

## 8. `internal/engine` (latency functions) · `internal/replica` · `internal/pool`

```go
// engine (add to the package; params.go is fixed)
func (p Params) LoadFactor(running int) float64            // 1 + (F-1)*(running-1)/(MaxNumSeqs-1), clamped to [1, F]; 1 if MaxNumSeqs == 1
func (p Params) PrefillTime(inputTokens, running int) time.Duration // (PrefillOverhead + PrefillPerToken*in) * LoadFactor
func (p Params) TokenInterval(running int) time.Duration  // InterTokenLatency * LoadFactor
func Jitter(d time.Duration, frac float64, rng *rand.Rand) time.Duration // d*(1+frac*N(0,1)), clamped to [0.5d, 1.5d]
func CountTokens(prompt string) int                       // whitespace-separated words, min 1
```

`internal/replica` — one SIMULATED OpenAI-compatible engine (real HTTP, simulated compute):
- `New(id string, p engine.Params, seed int64) *Replica`; `Handler() http.Handler`;
  `Start(now)` begins loading (state "starting" until StartupDuration elapses, then "ready");
  `BeginDrain()` (readiness false, in-flight finishes, new requests 503); `Drained() bool`;
  `State() string`; `Stats() Stats{Running, Waiting int}`.
- `POST /v1/completions` `{model, prompt, max_tokens (default 64), stream}`: always produces
  exactly max_tokens tokens (trace replay = ignore_eos). At most MaxNumSeqs run; the rest wait
  in the replica's own FCFS queue. Prefill sleeps PrefillTime(in, running); each token sleeps
  TokenInterval(running) using the running count at that moment. Stream = SSE
  `data: {"id","object":"text_completion","choices":[{"index":0,"text":"tok ","finish_reason":null}]}`,
  last chunk has `finish_reason:"length"` and `usage{prompt_tokens,completion_tokens,total_tokens}`,
  then `data: [DONE]`. Non-stream returns one JSON with `usage`. Client disconnect aborts.
- `GET /health/ready` (200 ready / 503 otherwise), `/health/live`, `GET /v1/models`,
  `GET /metrics`: `vllm:num_requests_running`, `vllm:num_requests_waiting`,
  `vllm:prompt_tokens_total`, `vllm:generation_tokens_total`, `vllm:request_success_total`,
  `vllm:time_to_first_token_seconds` (histogram), `vllm:e2e_request_latency_seconds`.

`internal/pool` — local stand-in for the model Deployment (replicas hosted in-process, each
with its own `http.Server` on `127.0.0.1:0`):
- `New(Config{Name string; Params engine.Params; MaxReplicas int; Seed int64}) *Pool`;
  `Scale(n int) error` (clamp [0, MaxReplicas]; scale-down drains starting replicas first, then
  newest ready ones; drained replicas are shut down); `Status() Status`; `Endpoints() []string`
  (ready base URLs, sorted); `Handler()`; `Close() error`.
- `Status{Desired, Ready, Starting, Draining int; Replicas []ReplicaStatus{ID, URL, State string; Running, Waiting int}}`
- HTTP: `GET /pool` → Status, `PUT /pool/scale` `{"replicas":n}`, `GET /endpoints`
  → `{"endpoints":[...]}`, `GET /healthz`.
- `Client{BaseURL string; HTTP *http.Client}` with `Scale(ctx, n)`, `Status(ctx)`, `Endpoints(ctx)`.
- Subcommands: `replica` (one replica on -addr) and `pool` (pool manager on -addr, -config).

## 9. `internal/workload` and `internal/eval`

```go
// workload
type Spec struct { Offset time.Duration; Tenant string; Priority model.Priority; InputTokens, OutputTokens int }
type LengthDist struct { Median, Sigma float64; Min, Max int } // log-normal
type PriorityMix struct { Critical, Standard, Sheddable float64 }
type TenantPattern struct {
    Tenant, Kind string // Kind: "steady" | "periodic" | "spike"
    BaseRPS, PeakRPS float64
    PeriodSeconds, BurstSeconds, PhaseSeconds float64 // periodic: burst of PeakRPS every Period
    SpikeStartSeconds, SpikeDurationSeconds float64   // spike: PeakRPS inside the window
    CV float64                                         // Gamma inter-arrival CV (1 = Poisson)
    Mix PriorityMix; In, Out LengthDist
}
type Scenario struct { Name string; Duration time.Duration; Tenants []TenantPattern; Samples []LengthSample }
func DefaultScenario() Scenario // A steady, B periodic, C spike (the problem statement's trace)
func ReclaimScenario() Scenario // A nearly idle, C spikes and borrows, then A returns (reclaim latency)
func Generate(s Scenario, seed int64) []Spec // deterministic, sorted by Offset; non-homogeneous Gamma arrivals
func RateAt(p TenantPattern, t float64) float64
type LengthSample struct{ In, Out int }
func LoadBurstGPT(path string) ([]LengthSample, error) // header: Timestamp,Session ID,Elapsed time,Model,Request tokens,Response tokens,Total tokens,Log Type; skips Response tokens == 0 (failures)
func LoadAzure(path string) ([]LengthSample, error)    // header: TIMESTAMP,ContextTokens,GeneratedTokens
// When Scenario.Samples is non-empty, lengths are drawn from it instead of LengthDist.
```

```go
// eval
type Record struct {
    ID uint64; Tenant string; Priority model.Priority
    Arrival, FirstToken, Done time.Duration // offsets from run start; zero when shed/failed
    Shed bool; ShedReason string; Failed bool
    InputTokens, OutputTokens int
}
type Sample struct { // one per second
    T time.Duration
    Ready, Starting, Desired int
    InFlight, Queued map[string]int
    CounterGap float64 // max-min VTC counter among backlogged tenants (fair mode), NaN otherwise
}
type RunData struct {
    Setup string; Run int; Seed int64
    Records []Record; Timeline []Sample
    SLOs map[string]model.SLO; GuaranteedSlots map[string]int
    Victims []string; Noisy string
    SpikeStart, SpikeEnd time.Duration
    ActionTimes []time.Duration     // protective actions: desired replicas up, or shed policy applied
    PolicyLatencies []time.Duration
    Owner string; OwnerReturn time.Duration // reclaim scenario (zero = not applicable)
}
type RunMetrics struct {
    VictimViolationPct, VictimShedAwareViolationPct float64 // spike window, victims; shed counted as miss in the second
    P99TTFT, P99TPOT map[string]float64 // seconds, completed requests
    NoisyShedPct float64
    ReactionTimeSec float64   // first ActionTime >= SpikeStart, minus SpikeStart; NaN if none
    ReplicaMinutes float64    // integral of (Ready+Starting) over the run / 60
    JainIndex float64         // spike window, x_i = min(1, attainment_i / target_i) over all tenants
    MaxCounterGap float64     // max CounterGap (NaN if never defined)
    ReclaimLatencySec float64 // offset from OwnerReturn of the owner's LAST request (arriving within eval.ReclaimHorizon = 120 s) that missed its TTFT bound; shed/failed/unfinished = miss; 0 if none; NaN if n/a
    PolicyLatencyMeanMs float64
    Completed, Shed, Failed int
}
func Compute(d RunData) RunMetrics
type Stat struct{ Mean, Std, CI95 float64; N int } // CI95 = t(0.975, N-1)*Std/sqrt(N); NaNs skipped
func Summarize(values []float64) Stat
func TCritical95(df int) float64 // exact table df 1..30, then 1.96
func Jain(xs []float64) float64
func Percentile(xs []float64, p float64) float64 // nearest-rank
// plus CSV/Markdown writers and a tiny SVG line/bar chart writer for results plots
```

## 10. `internal/k8s` and `deploy/`

```go
const Group, Version, Resource, Kind = "scaling.tenantscaler.dev", "v1alpha1", "tenantscalers", "TenantScaler"
var GVR = schema.GroupVersionResource{Group: Group, Version: Version, Resource: Resource}
type TenantScalerSpec struct { Pool string; GuaranteedReplicas, MaxReplicas, Priority int; SLO model.SLO }
type TenantScalerStatus struct { Grant, Demand int; Shedding bool; SLOSlack float64; Reason string; PolicyVersion, ObservedGeneration int64; LastUpdate metav1.Time }
type TenantScaler struct { metav1.TypeMeta; metav1.ObjectMeta; Spec TenantScalerSpec; Status TenantScalerStatus }
func FromUnstructured(u *unstructured.Unstructured) (TenantScaler, error)
func (t TenantScaler) TenantSpec() model.TenantSpec // Name = metadata.name
type Client struct { Dynamic dynamic.Interface; Kube kubernetes.Interface; Namespace string }
func NewClient(kubeconfig, namespace string) (*Client, error) // "" = in-cluster
func (c *Client) ListTenants(ctx context.Context, pool string) ([]TenantScaler, error)
func (c *Client) UpdateStatus(ctx context.Context, name string, st TenantScalerStatus) error
type DeploymentActuator struct { C *Client; Deployment string } // SetReplicas via scale subresource; Status from Deployment status → model.PoolObservation
type EndpointSliceDiscovery struct{...} // NewEndpointSliceDiscovery(c, service string, port int); Refresh(ctx); Endpoints() []string (ready only, "http://ip:port", sorted)
```
`deploy/k8s/`: namespace, CRD (structural OpenAPI schema, status subresource, printer
columns), RBAC, model-pool Deployment + Service (`tenantscaler replica`), gateway, controller,
three sample TenantScalers, `baselines/` (KEDA ScaledObject, CPU HPA), `kind-config.yaml`;
root `Dockerfile` (multi-stage, static binary). A Go test parses every manifest and validates
the sample tenants with `config.ValidateTenants`.

## 11. `internal/control` — controller core (pure, clock-agnostic)

```go
type Decision struct {
    Cycle int64; Time time.Time
    DesiredReplicas int       // clamp(sum grants, MinReplicas, capacity) before stabilization
    PoolReplicas int          // after stabilization: what the actuator applies
    Policy *policy.Policy     // nil when gateway coupling is disabled
    Records []model.DecisionRecord
    FailStatic bool
}
func NewCore(cfg config.Config, coldStart time.Duration) (*Core, error)
func (c *Core) SetTenants(specs []model.TenantSpec) error // Kubernetes mode (CRs can change)
func (c *Core) Step(obs model.Observation) Decision
func (c *Core) State() State; func (c *Core) Restore(State)
```
Step: if `!obs.OK` → fail-static (repeat the last PoolReplicas and last policy, records with
FailStatic and reason "metrics unavailable: holding N replicas and policy vK"). Otherwise per
tenant: reactive = `estimate.Replicas(rate, headroom, C)`; if forecasting is enabled the
tenant's forecaster observes the rate each cycle (one bucket = one cycle) and, when its
`Tracker` trusts it, forecast demand = `estimate.Replicas(max_{h=1..L} Forecast(h))` with
L = ceil((coldStart+cycle)/cycle); demand = max(reactive, forecast). Slack =
`estimate.Slack`. Arbitrate (Borrowing = !DisableBorrowing). Desired = clamp(sum grants,
MinReplicas, capacity); PoolReplicas = stabilizer.Next. Policy (unless
DisableGatewayCoupling): version+1, Weight = max(grant, Guaranteed) (≥ MinWeight; an idle
tenant keeps its guaranteed share at the gateway, so it is not starved when it returns — see
§19), Shed = Shedding,
RetryAfterSeconds from config, CriticalSlots = Guaranteed × SlotsPerReplica.

## 12. `internal/controller` — live runner around `control.Core`

```go
type MetricsSource interface{ Observe(ctx context.Context) (model.Observation, error) }
type Actuator interface {
    SetReplicas(ctx context.Context, n int) error
    Status(ctx context.Context) (model.PoolObservation, error)
}
type Publisher interface{ Publish(ctx context.Context, p policy.Policy) error }

type GatewaySource struct{ URLs []string; Window time.Duration; HTTP *http.Client }
// GET {url}/stats?window=<Window>; merge tenants across gateways: sum rates/queue/inflight/
// shed/completed, attainment weighted by completed, p99 = max.
type PrometheusSource struct{ BaseURL string; Window time.Duration; HTTP *http.Client }
// GET {BaseURL}/api/v1/query?query=... with (W = Window):
//   sum by (tenant)(rate(tenantscaler_gateway_arrival_weighted_tokens_total[W]))
//   sum by (tenant)(rate(tenantscaler_gateway_arrivals_total[W]))
//   sum by (tenant)(tenantscaler_gateway_queue_length) · sum by (tenant)(tenantscaler_gateway_inflight)
//   sum by (tenant)(increase(tenantscaler_gateway_completed_total{slo="met"}[W]))
//   sum by (tenant)(increase(tenantscaler_gateway_completed_total[W]))
//   histogram_quantile(0.99, sum by (tenant, le)(rate(tenantscaler_gateway_ttft_seconds_bucket[W])))
//   sum by (tenant)(increase(tenantscaler_gateway_shed_total[W]))
type PoolActuator struct{ Client *pool.Client }      // local mode
type HTTPPublisher struct{ URLs []string; HTTP *http.Client } // POST {url}/policy; error on non-2xx
type DecisionLog struct{...} // ring buffer (cap 2000) + optional JSONL file
func NewDecisionLog(capacity int, jsonlPath string) (*DecisionLog, error)
func (l *DecisionLog) Add(recs ...model.DecisionRecord)
func (l *DecisionLog) List(limit int, tenant string) []model.DecisionRecord // newest first
type Runner struct {
    Core *control.Core; Source MetricsSource; Actuator Actuator; Publisher Publisher // nil = no coupling
    Log *DecisionLog; Cycle time.Duration; Clock func() time.Time; Logger *slog.Logger
    BeforeCycle func(ctx context.Context) error   // e.g. reload tenants from CRs
    AfterDecision func(ctx context.Context, d control.Decision) // e.g. write CR status
}
func (r *Runner) RunOnce(ctx context.Context) control.Decision
func (r *Runner) Run(ctx context.Context) error // RunOnce every Cycle until ctx is done
func (r *Runner) Handler() http.Handler
// GET /decisions?limit=&tenant= · GET /status (last decision, cycle, pool, last errors) ·
// GET /healthz · GET /metrics (cycles_total, failstatic_total, desired/pool replicas, grant per tenant)
```
RunOnce: obs ← Source (error ⇒ obs.OK=false, fail-static); obs.Pool ← Actuator.Status;
d ← Core.Step(obs); Actuator.SetReplicas(d.PoolReplicas) unless fail-static; Publish(d.Policy)
when non-nil; Log.Add(d.Records); actuation/publish errors are kept in /status and logged.
Subcommand `controller`: local mode (`-config`, `-pool-url`, `-gateway` (comma list),
`-metrics gateway|prometheus`, `-prometheus-url`, `-cycle`, `-disable-borrowing`,
`-disable-coupling`, `-forecast`, `-decisions-file`) and Kubernetes mode (`-k8s`, `-kubeconfig`,
`-namespace`, `-deployment`, `-pool`, `-capacity`): BeforeCycle lists TenantScalers and calls
`Core.SetTenants`; AfterDecision writes each tenant's status.

## 13. `internal/gateway` — the fast loop

```go
type Discovery interface{ Endpoints() []string }
type StaticDiscovery []string
type PollingDiscovery struct{...} // NewPollingDiscovery(fetch func(context.Context) ([]string, error), every time.Duration); Start(ctx)
type Config struct {
    Mode            string // "fair" | "roundrobin" | "passthrough" | "partition"
    Tenants         []model.TenantSpec // SLOs (deadlines, attainment); unknown tenant → 400
    SlotsPerReplica int                // dispatch window per endpoint
    Discovery       Discovery
    UpstreamAPIKey  string             // "Authorization: Bearer" to upstream when set
    StatsWindow     time.Duration      // default 15 s; /stats?window= overrides
    PartitionShares map[string]int     // partition mode; default = GuaranteedReplicas
    DispatchTimeout time.Duration      // max queued wait before 503; default 120 s
    Seed            int64
    Logger          *slog.Logger
}
func New(cfg Config) (*Gateway, error)
func (g *Gateway) Handler() http.Handler
func (g *Gateway) SetPolicy(p policy.Policy) bool // same as POST /policy, for in-process use
func (g *Gateway) Stats(window time.Duration) Stats
func (g *Gateway) Close()
type Stats struct {
    Time time.Time; Mode string; PolicyVersion int64; PolicyLatencyMs float64
    Endpoints, Slots, InFlight, Queued int
    Tenants   []model.TenantObservation
    Scheduler []fairq.TenantState
}
```
- `POST /v1/completions`: tenant from `x-llm-d-inference-fairness-id` (alias `x-tenant-id`),
  priority from `x-priority` (class or integer; bad value → 400). Body `{prompt, max_tokens
  (default 64), stream}`; input tokens = `engine.CountTokens(prompt)`; deadline = arrival +
  tenant TTFT SLO; arrival weighted tokens = in + 2·max_tokens. Rejected → 429 JSON
  `{"error":{"type":"rate_limited","reason":...,"tenant":...}}` + `Retry-After` (whole seconds,
  ≥ 1). Admitted → wait for dispatch (client gone ⇒ Cancel), forward the body unchanged to
  `{endpoint}/v1/completions`, stream the response through with flushing, count output tokens
  from SSE chunks with non-empty `choices[0].text` (prefer final `usage.completion_tokens`),
  TTFT at the first token chunk, then `Complete`. Upstream error → 502; no endpoint within
  DispatchTimeout → 503.
- fair / roundrobin: `fairq` + dispatch window (≤ SlotsPerReplica in flight per endpoint,
  least-loaded endpoint first); a dispatcher goroutine wakes on arrival, completion, endpoint
  change and policy change. `saturated` (roundrobin) = no free slot at admission.
- passthrough (baselines with no smart gateway, like a plain Kubernetes Service): forward
  immediately to endpoints round-robin, no gateway queue, never sheds; replicas queue FCFS.
- partition (static partition baseline): endpoints sorted; tenant i owns a contiguous slice of
  PartitionShares[i] endpoints (leftovers dealt round-robin in tenant order); per-tenant FIFO
  with the dispatch window on its own endpoints only.
- `POST /policy` (validate; older version → 200 `{"applied":false}`), `GET /policy`,
  `GET /stats`, `GET /metrics` (`tenantscaler_gateway_arrivals_total{tenant,priority}`,
  `..._arrival_weighted_tokens_total{tenant}`, `..._shed_total{tenant,reason}`,
  `..._completed_total{tenant,slo}`, `..._ttft_seconds{tenant}` and `..._tpot_seconds{tenant}`
  histograms, `..._queue_length{tenant}`, `..._inflight{tenant}`, `..._policy_version`,
  `..._policy_propagation_seconds`, `..._endpoints`), `GET /healthz`.
- Policy propagation latency = receive time − `IssuedAt`.
Subcommand `gateway`: `-addr -config -mode -pool-url -endpoints (default $TS_UPSTREAM_ENDPOINTS)
-k8s-service -k8s-port -namespace -kubeconfig -slots`; API key from `$TS_UPSTREAM_API_KEY`.

## 14. `internal/sim`, `internal/experiment` — evaluation in virtual time

`sim.Run(Options) (eval.RunData, error)` is a discrete-event simulation (event heap ordered by
time then sequence) of: replicas (starting → ready after cold start; MaxNumSeqs slots;
internal FCFS waiting queue; draining), the gateway (same `fairq` code; passthrough /
partition / dispatch-window modes as in section 13), and the control plane (same
`control.Core` for S5–S7, same `autoscale.HPA/KEDA` for S1/S2/S4) ticking every cycle.
Setups: S1 CPU-HPA (slot occupancy as the CPU proxy) + passthrough · S2 queue-KEDA +
passthrough · S3 static partition (pool fixed at capacity) · S4 Flow-Control-equivalent
(roundrobin gateway + KEDA on gateway queue) · S5 TenantScaler without gateway coupling
(arbiter sets pool size, passthrough gateway) · S6 TenantScaler without borrowing · S7
TenantScaler full · S7F S7 + forecaster. Same scenario, seed, budget, engine and cycle for all.
Request service time: prefill = `PrefillTime(in, running)` with jitter, then
`(out-1) × TokenInterval(running at start)` (load fixed at start: documented approximation).
`experiment` writes `results/` (per-run CSV, `summary.md`, `summary.json`, SVG plots) and
supports `-runs`, `-cold-starts`, `-setups`, `-scenario default|reclaim`; `profile` finds
the per-replica capacity C (largest weighted token rate meeting the SLO).

**As built (wave 2 notes).** The gateway counts admitted requests that fail on the serving
side (dispatch timeout, upstream 5xx, broken stream) as completed SLO misses, strips a
trailing `/v1` from endpoint URLs (Ollama-style bases), and in partition mode answers queued
requests of a removed tenant with 503. In Kubernetes mode it reads tenants from TenantScaler
CRs (`Gateway.FollowTenants`). The controller's `HTTPPublisher` treats `applied:false` as a
stale-version error and `Runner.SeedPolicyVersion` restarts numbering above the newest version
in use. The simulator adds a sensitivity variant **S4b** (Flow-Control-equivalent gateway +
KEDA on outstanding requests) and models the gateway's 120 s dispatch timeout. The CPU-HPA
reads slot occupancy over `max(desired, ready+starting)` replicas in both the simulator and
the live `scalerun` (not-ready replicas count as idle, as in the Kubernetes replica calculator).
`estimate.Slack` is the smaller of the TTFT margin and the attainment margin.

## 15. `internal/loadgen`, `internal/scalerun`

`loadgen.Replay` sends a scenario's specs to one or more OpenAI-compatible targets and
records `eval.Record`s (real wall-clock offsets). Prompts are UNIQUE random words per
request (llama.cpp caches identical prompt prefixes; repeated prompts would flatter the
numbers); body adds `"ignore_eos": true` so the output length equals `max_tokens`.
`scalerun.Runner` is the live baseline autoscaler (KEDA on queue length or CPU-HPA on slot
occupancy) driving a pool.

## 16. Real replicas: `internal/llama` and pool backends (decisions Q5–Q8)

```go
// internal/pool
type Instance interface {
    ID() string
    URL() string
    State() string                          // replica.StateStarting|StateReady|StateDraining|StateStopped
    Stats() replica.Stats                   // Running, Waiting
    BeginDrain()
    WaitDrained(ctx context.Context) error   // in-flight work finished
    Close() error                           // stop now (kills a real process)
}
type Backend interface {
    Name() string                           // "simulated" | "llama"
    Simulated() bool
    Launch(id string, slot int) (Instance, error) // slot in [0, MaxReplicas): CPU-mask index
}
func NewSimulatedBackend(p engine.Params, seed int64) Backend // today's in-process replica
// pool.Config gains Backend (nil = simulated with Params); Status gains Backend string and
// Simulated bool; ReplicaStatus gains Slot int and ColdStartSeconds float64.
```
`internal/llama` — one real `llama-server` process per replica:
- `Launcher{Spec config.LlamaSpec; LogDir string}`; `Launch(id string, slot int) (*Process, error)`:
  resolve Server/Model (`$TS_LLAMA_SERVER`/`$TS_LLAMA_MODEL` fallback; clear error if missing),
  pick a free 127.0.0.1 port, start `llama-server -m MODEL -ngl GPULayers -t Threads
  -C CPUMasks[slot] --cpu-strict 1 -c Parallel*CtxPerSlot -np Parallel --metrics --host
  127.0.0.1 --port P` (+ `-dev none` when GPULayers == 0), log to `LogDir/<id>.log`.
- `*Process` implements `pool.Instance`: "starting" until `GET /health` returns 200 (records
  the real cold start), "ready", "draining", "stopped" (also when the process exits on its
  own); `Stats()` from `GET /metrics` (`llamacpp:requests_processing`,
  `llamacpp:requests_deferred`), cached ≤ 250 ms; `WaitDrained` polls until both are 0;
  `Close` kills the process and waits for it.
- **No orphans**: on Windows every child joins a Job Object with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (stdlib `syscall` + kernel32, build-tagged), so the OS
  kills replicas if TenantScaler dies; on Linux `Setpgid` + `Pdeathsig=SIGKILL`. Launched PIDs
  are appended to `.run/llama.pids`; `tenantscaler cleanup` kills any still alive.
- `--cache-ram` is passed explicitly (config `cacheRamMiB`, default 0 = off, capped at 1024):
  the b11898 default host prompt cache is 8192 MiB per replica, which four replicas could
  grow past the machine's RAM limits.
- The pool reconciles crashed replicas on its own (every 1 s, with backoff); `WaitDrained`
  enforces a 1 s drain grace; on Windows TenantScaler joins the kill-on-close job before its
  first launch.
- Default CPU masks: replica slot k → hex `3 << (2k)` (one physical core, two SMT threads).
  A replica launched while all slots are held (one is still draining) shares the draining
  replica's slot; documented as a short overlap.

## 17. `internal/realrun` — evaluation on REAL replicas (R27, R28)

`realrun.Run(Plan{Config, Setups, Runs, Scenario, OutDir, BaseSeed, Cooldown})` runs each
setup live, one at a time: fresh pool (llama backend, `InitialReplicas` started and READY
before traffic) + gateway (in-process `http.Server` on 127.0.0.1:0) + control plane
(controller `Runner` for S5/S7, `scalerun` KEDA for S2, none for S3 static partition at full
capacity), `loadgen.Replay` of the scenario at Speed 1, a 1 s sampler (pool status + gateway
stats → `eval.Sample`), action times (controller decisions / scalerun), then `eval.Compute`.
Everything is stopped and the guard re-checked between runs. Writes `OutDir/runs.csv`,
`summary.md`, `summary.json`, SVG plots, and per-run raw records. Setups: S2, S3, S5, S7
(names shared with `internal/sim`). The real scenario (`workload.RealScenario()`) is
scaled to the 4-replica CPU pool and lasts ~4 min (spike in the middle).
`profile` (real): drives one llama replica at concurrency 1..Parallel with the scenario's
length distribution, reports TTFT/TPOT/throughput per level, the capacity C (weighted
tokens/s at the highest level meeting the SLO) and the simulated-engine parameters that
reproduce the measurements (so the simulated appendix is calibrated to this machine).

## 18. `internal/demo` and dashboard — sequential A/B (decision Q7)

`demo` serves the dashboard. **Run A/B** replays the same recorded traffic (the real
scenario, shortened) first on the **baseline** stack (passthrough gateway + KEDA `scalerun`),
then on **TenantScaler** (fair gateway + controller), each on the full replica budget, one at
a time. Backend = llama when `$TS_LLAMA_SERVER` and `$TS_LLAMA_MODEL` exist, otherwise the
simulated engine with a visible **SIMULATED** badge. API: `GET /healthz`, `POST /api/ab/start`,
`POST /api/ab/stop`, `GET /api/state` (phase, progress, backend, per stack: 1 s samples of
replicas ready/starting, per-tenant p99 TTFT and SLO attainment over 10 s, rps, shed; final
`eval.RunMetrics` per finished stack), `GET /api/decisions?limit=` (TenantScaler controller
records), `GET /api/results` (real + simulated summaries if present), `GET /api/config`.
UI under `internal/web/static`: two panels (baseline | TenantScaler) filled as each run
progresses, Run A/B button, decision log with plain-English reasons, results tab.

## 19. Findings from the real runs (2026-10-26) and the changes they caused

Every change below was found on real llama.cpp replicas or by the calibrated simulator, fixed
with a regression test that fails without the fix, and re-measured.

| # | Evidence | Root cause | Change |
|---|---|---|---|
| F1 | Real smoke S7: victims A/B queued 7–19 s while C held all 16 slots; VTC counter gap ~3,800 (`.run/smoke-real` timeline) | A tenant with zero demand in a cycle got gateway weight MinWeight = 0.05, so its next requests were charged 20× and starved until C caught up | Weight = max(grant, guarantee) (`control.gatewayWeight`); fail-static records report the last grants (`TestIdleTenantKeepsGuaranteedWeight`) |
| F2 | Simulated S7 shed 41.5% of C at budget 10 although victims never missed (S4b: 3.9%) | Over-share shedding applied whenever policy.Shed, even with free slots | Shed over-share only while contended (`TestFairOverShareOnlyUnderContention`, `TestShedAfterPolicy`); S7 noisy shed 41.5% → 0.2%, victims still 0% |
| F3 | Real runs: 2 of 223 requests failed with 502 `EOF` | llama-server closes idle keep-alive connections after 5 s; the gateway kept them 90 s and Go does not resend a POST | Idle timeout 2 s + empty `X-Idempotency-Key` so the transport resends a request that failed on a reused connection before any response (`TestReplicaClosingIdleConnection`) |
| F4 | Profile calibration check: 12 deviations | One load factor for prefill and decode; on llama.cpp TTFT grows 1.17× and TPOT 4.3× from 1 to 4 running requests | `engine.prefillFactorUnderLoad`; the check now passes at every level (results/real/profile.md) |
| F5 | Calibrated sim, real traces: S7 victims 10.9 ± 4.8% vs static partition 7.3%; real smoke: B's bursts waited behind C's borrowed slots | Requests are not preempted: a borrower holds freed slots ~5 s while the guaranteed owner's burst waits; weights alone cannot hand slots back | Slot reservation for shedding tenants (§7); ablation `S7nr-ts-no-reservation` |

F5 trade-off (calibrated / default simulator, 20 seeds, S7 victim violation %, mean ± 95% CI):
with reservation `real` 5.9 ± 3.7 (without 10.9 ± 4.8), `real-demo` 2.7 ± 2.5 (6.5 ± 3.2),
reclaim latency at budget 6 28.8 ± 18.4 s (52.5 ± 22.6 s); but default scenario at budget 6
2.1 ± 1.5 (0.7 ± 0.4) and the reclaim scenario at budget 6 6.2 ± 1.4 (3.0 ± 0.9), where the
periodic victim B is itself over its guarantee during its bursts and is limited to spare
slots, and the noisy tenant is shed more. Reservation is kept because it makes a guarantee hold
when service times are long relative to the TTFT SLO (the real testbed); the ablation is
reported next to S7.

## 20. CPU pinning, replica scaling curve and the SLO derivation (BRIEF-v2 N2)

**Topology** (`internal/topology`, Windows `GetLogicalProcessorInformationEx`): physical cores
with their efficiency class; P-cores = the highest class. On the development machine
(i5-14450HX): 6 P-cores with two hardware threads each (logical 0–11) and 4 E-cores (12–15).

**Pinning** (decision Q13): replica slot k runs on the k-th P-core, both hardware threads
(`llama.Launcher.cpuMask`, unless `backend.llama.cpuMasks` is set). On Windows the replica is
created suspended, its process affinity is set to that core, then it is resumed, because a
child inherits the parent's affinity and llama-server's `-C` thread affinity must lie inside
its process affinity. Real runs and profiles pin their own process (gateway, controller and
load generator) to every CPU the budget's replicas do not use (`llama.PinServices`), and record
the layout in their labels. CPU frequency is sampled with `typeperf` ("% Processor Performance",
effective frequency as a percentage of nominal); CPU temperature is not readable without
administrator rights (the ACPI thermal zones that are readable are not CPU sensors).

**Scaling curve** (`realrun.ScalingCurve`, `tenantscaler profile -scaling 4`): for k = 1..4,
k pinned replicas are loaded at once, each closed-loop at concurrency = slots per replica (the
gateway's dispatch window), in one common window; throughput is summed and percentiles pooled.

**Derivation rules, fixed before the measurement** (`realrun.DeriveBudget`):
- TTFT SLO: p99 ≤ 2 s (BRIEF-v2).
- TPOT SLO: 1.25 × the pooled p99 TPOT with the whole budget loaded at the dispatch window,
  rounded up to 10 ms. Rationale: that is the best service the budget can give (no queueing in
  front of the replicas); with a 25% margin for jitter, a healthy full pool meets the SLO, so a
  violation in an experiment measures contention and queueing, not the engine's own speed.
- C (`replicaCapacityTokensPerSec`): the same level's pooled weighted tokens/s divided by the
  number of replicas, i.e. one replica's capacity while the whole budget is busy (interference
  included). The single-replica profile's C is reported alongside for reference.
- The full budget's pooled p99 TTFT must meet the TTFT SLO; otherwise the budget is too large
  for the SLO and is reduced (BRIEF-v2 Q13: "keep budget = 4 unless the new profile says
  otherwise").

**Measured 2026-10-27** (REAL; `results/real/scaling.md`, `scaling.csv`, `profile.md`; replicas on
CPUs 0-1 | 2-3 | 4-5 | 6-7, services on 8-15; 60 s per level after 5 s warm-up, 4 concurrent
requests per replica):

| replicas | weighted tok/s total | per replica | efficiency | p99 TTFT s | p99 TPOT ms |
|---|---|---|---|---|---|
| 1 | 133.6 | 133.6 | 1.000 | 1.972 | 133.9 |
| 2 | 261.3 | 130.7 | 0.978 | 1.845 | 137.1 |
| 3 | 371.4 | 123.8 | 0.927 | 2.072 | 134.9 |
| 4 | 466.7 | 116.7 | 0.874 | 1.754 | 156.6 |

Applying the rules: TPOT SLO = 1.25 × 156.6 ms → **200 ms**; C = 466.7 / 4 → **117**; the full
budget meets TTFT p99 ≤ 2 s (1.754 s), and the 4th replica still adds 26% (371.4 → 466.7), so the
budget stays **4**. Caveats: each p99 rests on 39–137 requests, so it is close to the sample
maximum (the 3-replica level's 2.07 s is above 2 s while the 4-replica level's is below); the
single-replica profile (cold start 605 ms, C = 136 at concurrency 4) and its engine calibration
are within 25% at concurrency 1–5, with one deviation at concurrency 6 (p50 TTFT +28%), outside
the dispatch window.

## 21. Offered demand, the S8 ablation and the death-spiral finding (BRIEF-v2 N3, Q11)

The gateway's `/stats` (and the simulator) report two rates per tenant: **offered** weighted
tokens (`arrivalWeightedTokensPerSec`, every arrival, shed ones included, output counted as
`max_tokens`) and **admitted** weighted tokens (`admittedWeightedTokensPerSec`, arrivals minus
shed). The controller always used the offered rate; `controller.naiveDemand: true` switches it to
the admitted rate. **S8** (`S8-ts-naive-demand`) is S7 with that switch (simulated appendix only).

Tests were written before the switch existed (commit c3d68df). `TestNaiveDemandSpirals` (control
core in a closed loop with a gateway that sheds C's whole sheddable share while flagged) shows
the mechanism: under naive demand C's grant falls 8 → 4 and the desired pool 10 → 6 while C's
offered load is unchanged; offered demand holds 8 and 10. The simulator test, however, found
**no spiral in the full system**: S8 matched S7 (PLAN.md Q11-R has the numbers). Reasons:
TenantScaler's shed decision is itself "demand > grant", computed from the same estimate, so a
low admitted-only reading switches shedding off and the next reading recovers; and over-share
shedding removes only the excess while the pool is contended, so admitted load stays at or
above the budget-capped grant. A spiral needs shedding that is independent of the scaling
signal; S4 (saturation shedding + KEDA on the gateway queue) has that shape and scales down in
the middle of a spike (`TestFlowControlMetricBlindSpot`). The test now records the finding.

## 22. Real harness (BRIEF-v2 N4): S4, readiness hold, interleaving, tokenizer counting

- **Setups:** `realrun` runs S2, S3, S4, S5, S7 by default (decision Q10); S4 is the
  Flow-Control-equivalent baseline (roundrobin gateway, KEDA on the gateway queue).
- **Readiness hold T** (`realrun -hold`, `pool.WithReadinessHold`, decision Q12): every replica,
  the warm pool included, starts and loads the model for real, but the pool reports it as
  starting, keeps it out of the gateway's discovery and counts it as starting for KEDA and the
  controller until T after its launch. Its cold start is then max(real, T). Every report labels
  it "injected load delay (emulates network model load); inference is real". The headline runs
  use T = 30 s and T = 0.
- **Run order** (decision Q14): inside every run index the setups run in a seeded shuffle
  (`runOrder`, PCG keyed by the base seed and the run index); each run records its position
  (`order`) and global `sequence`. `summary.md` lists every run with its main metrics, the mean
  ± 95% CI (Student t, `eval.Summarize`) per setup, and a drift check (each setup's first vs
  last run on victim violation and CPU frequency).
- **CPU frequency** is sampled at each run's start and end (`typeperf`, % of nominal); CPU
  temperature is reported as not readable.
- **Token counting** (decision Q17): a real stack's gateway counts input tokens with a replica's
  own tokenizer (`POST /tokenize`, `llama.TokenCounter`, rotating over ready endpoints, 2 s
  timeout); output tokens are `max_tokens`, which the load generator forces (`ignore_eos`).
  When the tokenizer cannot answer, the gateway counts words and the run reports the number of
  such fallbacks (expected 0). The simulated path counts words by design. The load generator's
  records use the server's `usage` counts.

## 23. Live multi-laptop demo (BRIEF-v2 N5, decision Q16)

- **Keys:** `TS_TENANT_KEYS="A:standard=<key>,B:standard=<key>,C:sheddable=<key>"` (`gateway.ParseTenantKeys`).
  A key decides the tenant and its highest priority; a priority header may only lower it; a
  tenant header (including `x-llm-d-inference-fairness-id`) is ignored when keys are set (the key
  wins) and the answer names the tenant used (`X-TenantScaler-Tenant`).
- **Two listeners:** the gateway's full handler (`/policy`, `/stats`, `/metrics`) stays on
  loopback for the controller and the dashboard; `stack.Options.PublicAddr` adds a listener with
  `gateway.PublicHandler` (completions and `/healthz` only, keys required).
- **Dashboard:** `tenantscaler demo -live 0.0.0.0:8081` runs the TenantScaler stack until Stop,
  with the banner "LIVE DEMO — live demo, not the experiment" (`State.live`, `liveAddress`).
- **Teammates:** `tenantscaler loadgen -gateway http://<ip>:8081 -key <key> -profile
  steady|periodic|spike [-spike-now]` (cross-compiled into `dist/` by `scripts/build-dist.*`)
  sends one tenant's pattern from the real scenario and prints a line every 10 s.
- **Network:** `scripts/demo/firewall.ps1` adds one inbound TCP rule on the Private profile
  (and removes it with `-Remove`); it is run only with the owner's approval. Headline
  experiments never use this path: they use the local, pinned load generator.

