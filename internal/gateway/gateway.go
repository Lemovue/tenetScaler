// Package gateway is TenantScaler's fast loop: an OpenAI-compatible HTTP
// gateway in front of the shared replica pool.
//
// Every POST /v1/completions names its tenant (x-llm-d-inference-fairness-id)
// and priority class (x-priority). In fair and roundrobin mode the request
// is admitted or shed by the fairq scheduler and then HELD in the gateway
// until a replica slot is free: at most SlotsPerReplica requests are in
// flight per replica (the dispatch window), so the scheduler, not the
// replicas' FCFS queues, decides who is served next. A single dispatcher
// goroutine, woken by arrivals, completions, endpoint changes and policy
// changes, hands queued requests to the least-loaded replica. Responses are
// streamed through; the gateway measures TTFT and TPOT itself and counts
// output tokens from the SSE stream. The baselines passthrough (forward at
// once, like a Kubernetes Service) and partition (static per-tenant replica
// slices) share the same plumbing.
//
// The controller pushes a versioned policy (POST /policy) and reads windowed
// per-tenant observations (GET /stats or Prometheus GET /metrics).
//
// Upstreams can be the simulated replicas of package replica or real
// OpenAI-compatible servers such as llama.cpp's llama-server: the gateway
// forwards request bodies unchanged and relays response headers, including
// X-TenantScaler-Simulated.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/metrics"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// Gateway modes (Config.Mode).
const (
	// ModeFair is TenantScaler: weighted virtual token counters across
	// tenants (fairq.ModeFair) behind the dispatch window.
	ModeFair = "fair"
	// ModeRoundRobin is the llm-d Flow-Control-equivalent baseline
	// (fairq.ModeRoundRobin) behind the same dispatch window.
	ModeRoundRobin = "roundrobin"
	// ModePassthrough forwards every request at once, round robin over the
	// endpoints, like a plain Kubernetes Service: no gateway queue, no
	// shedding; requests queue FCFS inside the replicas.
	ModePassthrough = "passthrough"
	// ModePartition is the static-partition baseline: each tenant owns a
	// fixed slice of the endpoints and is served FIFO on them only.
	ModePartition = "partition"
)

// ParseMode validates a gateway mode (case-insensitive).
func ParseMode(s string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(s))
	switch m {
	case ModeFair, ModeRoundRobin, ModePassthrough, ModePartition:
		return m, nil
	}
	return "", fmt.Errorf("invalid gateway mode %q: want fair|roundrobin|passthrough|partition", s)
}

// Defaults applied by New to zero-valued Config fields, and limits.
const (
	// DefaultStatsWindow is the observation window of Stats and GET /stats.
	DefaultStatsWindow = 15 * time.Second
	// MaxStatsWindow bounds the observation window; older events are pruned.
	MaxStatsWindow = 5 * time.Minute
	// DefaultDispatchTimeout is the longest a request waits for a replica
	// slot before the gateway answers 503.
	DefaultDispatchTimeout = 120 * time.Second
	// DefaultMaxTokens is assumed for accounting when a request omits
	// max_tokens (the simulated replica's default too).
	DefaultMaxTokens = 64
)

// Request headers read by the gateway.
const (
	// TenantHeader carries the tenant (llm-d's fairness id).
	TenantHeader = "x-llm-d-inference-fairness-id"
	// TenantHeaderAlias is accepted when TenantHeader is absent.
	TenantHeaderAlias = "x-tenant-id"
	// PriorityHeader carries "critical", "standard", "sheddable" or an
	// integer (model.ParsePriority); absent means standard.
	PriorityHeader = "x-priority"
)

// tickEvery is the dispatcher's safety-net wake-up: it re-reads the
// endpoints and retries dispatch even when no event arrived.
const tickEvery = 100 * time.Millisecond

// Config configures a Gateway.
type Config struct {
	// Mode is one of the Mode constants; empty means ModeFair.
	Mode string
	// Tenants are the known tenants and their SLOs (TTFT deadline and SLO
	// attainment). Requests of any other tenant get 400.
	Tenants []model.TenantSpec
	// SlotsPerReplica is the dispatch window: the most requests in flight
	// per endpoint (>= 1).
	SlotsPerReplica int
	// Discovery lists the ready replica endpoints (required).
	Discovery Discovery
	// UpstreamAPIKey, when set, is sent upstream as "Authorization: Bearer".
	UpstreamAPIKey string
	// TenantKeys maps API keys to grants (ParseTenantKeys). When set,
	// POST /v1/completions needs "Authorization: Bearer <key>" and the key
	// decides the tenant and caps the priority. AdminKey, when set, is
	// needed on /policy, /stats and /metrics. Both are for gateways
	// reachable from outside the host (see also PublicHandler).
	TenantKeys map[string]KeyGrant
	AdminKey   string
	// TokenCounter counts a prompt's input tokens with a replica's own
	// tokenizer (decision Q17; llama.TokenCounter). Nil means whitespace
	// words, the simulated engine's tokenizer (engine.CountTokens).
	TokenCounter func(ctx context.Context, endpoint, prompt string) (int, error)
	// StatsWindow is the default observation window of Stats; zero means
	// DefaultStatsWindow; it is capped at MaxStatsWindow.
	StatsWindow time.Duration
	// PartitionShares is each tenant's number of endpoints in partition
	// mode; a tenant missing from the map gets its GuaranteedReplicas.
	PartitionShares map[string]int
	// DispatchTimeout is the longest a request waits for a replica slot;
	// zero means DefaultDispatchTimeout.
	DispatchTimeout time.Duration
	// Seed seeds the Retry-After jitter.
	Seed int64
	// Logger receives operational logs; nil discards them.
	Logger *slog.Logger
}

// Gateway is the fast loop. Create it with New, serve Handler, and Close it
// when done.
type Gateway struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	start   time.Time // bounds the Stats window right after a start
	reg     *prometheus.Registry
	metrics *gatewayMetrics
	obs     *observations
	client  *http.Client
	handler http.Handler
	nextID  atomic.Uint64
	// tokRR rotates the tokenizer endpoint; tokFallbacks counts prompts
	// counted in words because the tokenizer was unavailable.
	tokRR        atomic.Uint64
	tokFallbacks atomic.Int64

	ctx       context.Context // canceled by Close
	cancel    context.CancelFunc
	wake      chan struct{} // buffered(1): dispatcher wake-up
	done      chan struct{} // closed when the dispatcher exits
	unsub     func()
	closeOnce sync.Once

	mu sync.Mutex
	// tenants are the known tenants; order is their configured order.
	tenants map[string]model.TenantSpec
	order   []string
	// sched is the scheduler in fair, roundrobin and passthrough mode;
	// parts holds one FIFO scheduler per tenant in partition mode.
	sched *fairq.Scheduler
	parts map[string]*fairq.Scheduler
	// pending maps queued requests to their waiting handlers.
	pending map[*fairq.Request]*pending
	// endpoints is the current ready set (sorted); load counts in-flight
	// requests per endpoint, including endpoints that have since vanished.
	endpoints []string
	load      map[string]int
	// down excludes endpoints that just refused a request, until the time.
	down map[string]time.Time
	// owners is each tenant's endpoint slice in partition mode.
	owners map[string][]string
	rr     int // passthrough round-robin cursor
	counts map[string]*tenantCounts
	pol    policy.Policy
	polLat time.Duration
}

// tenantCounts are a tenant's live queue and in-flight counts.
type tenantCounts struct{ queued, inflight int }

// New validates cfg, starts the dispatcher and returns the gateway. Until a
// policy arrives every tenant has weight 1 and nothing is shed
// (policy.Equal).
func New(cfg Config) (*Gateway, error) { return newGateway(cfg, time.Now) }

// newGateway is New with an injectable clock, which tests use to check the
// time accounting (Stats windows, policy propagation latency) exactly.
func newGateway(cfg Config, clock func() time.Time) (*Gateway, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeFair
	}
	mode, err := ParseMode(cfg.Mode)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", err)
	}
	cfg.Mode = mode
	switch {
	case cfg.SlotsPerReplica < 1:
		return nil, fmt.Errorf("gateway: SlotsPerReplica must be >= 1, got %d", cfg.SlotsPerReplica)
	case cfg.Discovery == nil:
		return nil, fmt.Errorf("gateway: Discovery is required")
	case cfg.DispatchTimeout < 0 || cfg.StatsWindow < 0:
		return nil, fmt.Errorf("gateway: DispatchTimeout and StatsWindow must be >= 0")
	}
	for t, n := range cfg.PartitionShares {
		if n < 0 {
			return nil, fmt.Errorf("gateway: partition share of tenant %q must be >= 0, got %d", t, n)
		}
	}
	tenants, order, err := indexTenants(cfg.Tenants)
	if err != nil {
		return nil, err
	}
	if err := validateKeys(cfg, tenants); err != nil {
		return nil, err
	}
	cfg.TenantKeys = maps.Clone(cfg.TenantKeys)
	if cfg.StatsWindow == 0 {
		cfg.StatsWindow = DefaultStatsWindow
	}
	cfg.StatsWindow = min(cfg.StatsWindow, MaxStatsWindow)
	if cfg.DispatchTimeout == 0 {
		cfg.DispatchTimeout = DefaultDispatchTimeout
	}
	cfg.PartitionShares = maps.Clone(cfg.PartitionShares)
	cfg.Tenants = slices.Clone(cfg.Tenants)
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	g := &Gateway{
		cfg: cfg, log: log, now: clock, start: clock(), reg: metrics.NewRegistry(), obs: newObservations(),
		client: newUpstreamClient(),
		wake:   make(chan struct{}, 1), done: make(chan struct{}),
		parts: map[string]*fairq.Scheduler{}, pending: map[*fairq.Request]*pending{},
		load: map[string]int{}, down: map[string]time.Time{}, counts: map[string]*tenantCounts{},
		pol: policy.Equal(order),
	}
	g.ctx, g.cancel = context.WithCancel(context.Background())
	g.metrics = newGatewayMetrics(g.reg)
	switch mode {
	case ModeFair, ModeRoundRobin:
		g.sched = fairq.New(fairq.Config{Mode: fairq.Mode(mode), Seed: cfg.Seed})
	case ModePassthrough:
		// One unbounded FIFO: passthrough never sheds; requests wait here
		// only while no endpoint exists at all.
		g.sched = fairq.New(fairq.Config{Mode: fairq.ModeFCFS, MaxQueuePerTenant: 1 << 30, Seed: cfg.Seed})
	}
	if g.sched != nil {
		g.sched.SetPolicy(g.pol)
	}
	g.setTenantsLocked(tenants, order)
	g.handler = g.routes()
	g.refreshEndpoints()
	if n, ok := cfg.Discovery.(Notifier); ok {
		g.unsub = n.Subscribe(g.kick)
	}
	go g.run()
	return g, nil
}

// Handler returns the gateway's HTTP API: POST /v1/completions,
// POST and GET /policy, GET /stats, GET /metrics and GET /healthz.
func (g *Gateway) Handler() http.Handler { return g.handler }

func (g *Gateway) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/completions", g.handleCompletions)
	mux.Handle("POST /policy", g.adminOnly(http.HandlerFunc(g.handlePostPolicy)))
	mux.Handle("GET /policy", g.adminOnly(http.HandlerFunc(g.handleGetPolicy)))
	mux.Handle("GET /stats", g.adminOnly(http.HandlerFunc(g.handleStats)))
	mux.Handle("GET /metrics", g.adminOnly(metrics.Handler(g.reg)))
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return mux
}

// Close stops the dispatcher. Queued requests get 503 and in-flight
// upstream calls are canceled. Close is idempotent; it does not stop the
// Discovery, whose owner does.
func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		g.cancel()
		<-g.done
		if g.unsub != nil {
			g.unsub()
		}
		g.client.CloseIdleConnections()
	})
}

// kick wakes the dispatcher without blocking.
func (g *Gateway) kick() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// ms converts a duration to fractional milliseconds.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
