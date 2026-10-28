package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
)

// observeDecisionsEvery is how often an observing run reads the observed
// controller's decision log, and observeDecisionsLimit how many records.
const (
	observeDecisionsEvery = 5 * time.Second
	observeDecisionsLimit = 300
)

// ObserveTarget is a TenantScaler running elsewhere: in the k3s cluster of
// deploy/k3s, reached through scripts/k3s/expose.sh.
type ObserveTarget struct {
	// GatewayURL serves GET /stats, which needs AdminKey; ControllerURL
	// serves GET /status and GET /decisions.
	GatewayURL, ControllerURL string
	// BaselineGatewayURL, when set, is the side-by-side baseline's gateway
	// (deploy/k3s/side-by-side), shown in the baseline panel. Its /stats
	// (same AdminKey) gives the per-tenant statistics and the ready
	// replicas; KEDA's target is not read, so that panel shows no
	// starting replicas.
	BaselineGatewayURL string
	AdminKey           string
}

// ObservedBackend describes an observed cluster for the dashboard's
// labels: its replicas are the simulated engine of deploy/k8s, or with llama
// the llama.cpp pods of deploy/k3s/llama. cfg (from configPath) only sizes
// the dashboard's axes.
func ObservedBackend(cfg config.Config, configPath string, llama bool) BackendInfo {
	b := BackendInfo{
		Kind: "kubernetes", Simulated: true,
		Label: "KUBERNETES (k3s): SIMULATED replicas",
		Detail: "TenantScaler runs in the Kubernetes cluster: its controller scales the model-pool Deployment and its gateway " +
			"serves the teammates' traffic. This dashboard only reads the cluster's gateways (/stats) and controller " +
			"(/status, /decisions). The replicas are the SIMULATED engine, so latencies are simulated.",
		Reason:   "observing a cluster (demo -observe-gateway)",
		Replicas: cfg.Pool.CapacityReplicas, ConfigPath: configPath,
	}
	if llama {
		b.Simulated = false
		b.Label = "KUBERNETES (k3s): REAL llama.cpp replicas"
		b.Detail = "TenantScaler runs in the Kubernetes cluster: its controller scales the model-pool Deployment of llama.cpp pods " +
			"(official server image b11913, Qwen2.5-0.5B-Instruct Q4_K_M, 2 CPUs each) and its gateway serves the teammates' " +
			"traffic. This dashboard only reads the cluster's gateways (/stats) and controller (/status, /decisions). " +
			"Inference is real, but the pods run unpinned inside a WSL VM: latencies are not comparable with the pinned " +
			"bare-metal results."
	}
	return b
}

// StartObserve starts an observing run in the background. It starts no
// stack: every SampleEvery it reads the observed gateway's /stats and the
// controller's /status, and every few seconds the controller's decision
// log, until StopAB. A cluster's tenants are TenantScaler names, which
// Kubernetes keeps lower case (a, b, c); each is shown as the configured
// tenant of the same name in any case (A, B, C), so the per-tenant charts
// apply. It returns ErrBusy while a run is in progress and ErrClosed after
// Close.
func (s *Server) StartObserve(t ObserveTarget) (int, error) {
	if t.GatewayURL == "" || t.ControllerURL == "" {
		return 0, errors.New("demo: observing needs the gateway and the controller URL")
	}
	t.GatewayURL, t.ControllerURL = strings.TrimRight(t.GatewayURL, "/"), strings.TrimRight(t.ControllerURL, "/")
	t.BaselineGatewayURL = strings.TrimRight(t.BaselineGatewayURL, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.run != nil && !s.run.finished() {
		return 0, ErrBusy
	}
	s.runs++
	target := t
	s.observing = &target
	ctx, cancel := context.WithCancel(context.Background())
	r := newRun(s.runs, s.opt.Seed, &s.opt, s.roles, cancel)
	r.observe = &t
	if t.BaselineGatewayURL == "" {
		r.stacks[0].status = StatusSkipped // no baseline in the cluster to show
	}
	r.message = "Observing the TenantScaler in the Kubernetes cluster"
	s.run = r
	go r.executeObserve(ctx)
	return s.runs, nil
}

// observedStatus is the part of the controller's /status a sample needs.
type observedStatus struct {
	Pool     model.PoolObservation `json:"pool"`
	Decision *struct {
		PoolReplicas int `json:"poolReplicas"`
	} `json:"decision"`
}

// executeObserve samples the observed TenantScaler until the run is
// stopped. Ready replicas come from the gateway (its EndpointSlice view,
// refreshed every 2 s), the pool size the controller last applied from its
// /status (once per control cycle); Starting is the difference.
func (r *abRun) executeObserve(ctx context.Context) {
	defer close(r.done)
	defer r.cancel()
	const i = 1 // the TenantScaler panel
	log := r.opt.Logger.With("observeRun", r.id)
	log.Info("observing", "gateway", r.observe.GatewayURL, "controller", r.observe.ControllerURL)
	names := shownNames(r.opt.Config.Tenants)
	start := time.Now()
	r.mu.Lock()
	sr := r.stacks[i]
	sr.status, sr.simulated, sr.trafficStart, sr.gatewayURL = StatusRunning, r.opt.Backend.Simulated, start, r.observe.GatewayURL
	r.decStart = start
	r.message = fmt.Sprintf("Observing the TenantScaler in the Kubernetes cluster: gateway %s, controller %s",
		r.observe.GatewayURL, r.observe.ControllerURL)
	baseline := r.observe.BaselineGatewayURL != ""
	if baseline {
		b := r.stacks[0]
		b.status, b.simulated, b.trafficStart, b.gatewayURL = StatusRunning, r.opt.Backend.Simulated, start, r.observe.BaselineGatewayURL
		r.message = fmt.Sprintf("Observing the Kubernetes cluster side by side: TenantScaler gateway %s (controller %s), baseline gateway %s (KEDA)",
			r.observe.GatewayURL, r.observe.ControllerURL, r.observe.BaselineGatewayURL)
	}
	r.mu.Unlock()

	tick := time.NewTicker(r.opt.SampleEvery)
	defer tick.Stop()
	var status observedStatus // the last one read; zero until then
	var decided time.Time
	for {
		gs, err := r.gatewayStats(ctx, r.observe.GatewayURL)
		if err != nil && ctx.Err() == nil {
			log.Debug("observed gateway stats unavailable", "err", err)
		}
		var st observedStatus
		if err := r.getObserved(ctx, r.observe.ControllerURL+"/status", &st); err == nil {
			status = st
		} else if ctx.Err() == nil {
			log.Debug("observed controller status unavailable", "err", err)
		}
		smp := eval.Sample{T: time.Since(start), Ready: status.Pool.ReadyReplicas, Desired: status.Pool.DesiredReplicas,
			InFlight: map[string]int{}, Queued: map[string]int{}, CounterGap: math.NaN()}
		if status.Decision != nil {
			smp.Desired = status.Decision.PoolReplicas
		}
		if gs != nil {
			smp.Ready = gs.Endpoints
			for k := range gs.Tenants {
				o := &gs.Tenants[k]
				o.Tenant = names.shown(o.Tenant)
				smp.InFlight[o.Tenant], smp.Queued[o.Tenant] = o.InFlight, o.QueueLength
			}
		}
		smp.Starting = max(smp.Desired-smp.Ready, 0)
		r.addSample(i, smp, liveSample(smp, gs), gs)
		if baseline {
			bs, err := r.gatewayStats(ctx, r.observe.BaselineGatewayURL)
			if err != nil && ctx.Err() == nil {
				log.Debug("observed baseline gateway stats unavailable", "err", err)
			}
			bsmp := eval.Sample{T: time.Since(start), InFlight: map[string]int{}, Queued: map[string]int{}, CounterGap: math.NaN()}
			if bs != nil {
				bsmp.Ready, bsmp.Desired = bs.Endpoints, bs.Endpoints // KEDA's target is not read
				for k := range bs.Tenants {
					o := &bs.Tenants[k]
					o.Tenant = names.shown(o.Tenant)
					bsmp.InFlight[o.Tenant], bsmp.Queued[o.Tenant] = o.InFlight, o.QueueLength
				}
			}
			r.addSample(0, bsmp, liveSample(bsmp, bs), bs)
		}

		if time.Since(decided) >= observeDecisionsEvery {
			decided = time.Now()
			var recs []model.DecisionRecord
			url := fmt.Sprintf("%s/decisions?limit=%d", r.observe.ControllerURL, observeDecisionsLimit)
			if err := r.getObserved(ctx, url, &recs); err == nil {
				for k := range recs {
					recs[k].Tenant = names.shown(recs[k].Tenant)
				}
				r.mu.Lock()
				r.decisions = recs
				r.mu.Unlock()
			} else if ctx.Err() == nil {
				log.Debug("observed decisions unavailable", "err", err)
			}
		}

		select {
		case <-ctx.Done():
			r.mu.Lock()
			sr.status, r.phase = StatusStopped, PhaseStopped
			if baseline {
				r.stacks[0].status = StatusStopped
			}
			r.message = "Stopped observing: the cluster keeps running"
			r.mu.Unlock()
			log.Info("observing stopped")
			return
		case <-tick.C:
		}
	}
}

// getObserved GETs url and decodes its JSON body into v.
func (r *abRun) getObserved(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := r.stats.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// observedNames maps an observed tenant name to the configured tenant it
// is shown as: the one with the same name in any case.
type observedNames map[string]string

func shownNames(tenants []model.TenantSpec) observedNames {
	m := make(observedNames, len(tenants))
	for _, t := range tenants {
		m[strings.ToLower(t.Name)] = t.Name
	}
	return m
}

func (m observedNames) shown(name string) string {
	if s, ok := m[strings.ToLower(name)]; ok {
		return s
	}
	return name
}
