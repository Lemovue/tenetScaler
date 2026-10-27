package sim

import (
	"fmt"
	"math"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/control"
	"tenantscaler/internal/policy"
)

// initControl builds the setup's control plane. Every scaler is capped at
// the pool capacity (the budget).
func (s *simulator) initControl() error {
	switch s.spec.scaler {
	case scalerHPA:
		if err := s.st.HPA.Validate(); err != nil {
			return err
		}
		s.hpa = autoscale.NewHPA(s.st.HPA, s.st.InitialReplicas)
	case scalerKEDATotal, scalerKEDAGateway, scalerKEDAOutstanding:
		if err := s.st.KEDA.Validate(); err != nil {
			return err
		}
		s.keda = autoscale.NewKEDA(s.st.KEDA, s.st.InitialReplicas)
	case scalerCore:
		core, err := control.NewCore(s.cfg, s.st.ColdStart)
		if err != nil {
			return fmt.Errorf("%s: %w", s.o.Setup, err)
		}
		s.core = core
	}
	return nil
}

// tick runs one control cycle: observe, decide, scale, and send the
// policy (if any) on its way to the gateway.
func (s *simulator) tick() {
	s.diag.ControlCycles++
	now := s.clock()
	n := s.target
	switch s.spec.scaler {
	case scalerHPA:
		// Mean slot occupancy over the cycle, the CPU-utilisation proxy
		// (NaN without samples: the HPA then holds).
		occ := math.NaN()
		if s.occN > 0 {
			occ = s.occSum / float64(s.occN)
		}
		n = s.hpa.Step(now, s.target, occ)
	case scalerKEDATotal:
		total := 0
		for _, t := range s.tenants {
			total += t.gwQueued + t.waiting
		}
		n = s.keda.Step(now, s.target, float64(total))
	case scalerKEDAGateway:
		n = s.keda.Step(now, s.target, float64(s.sched.Len()))
	case scalerKEDAOutstanding:
		total := s.sched.Len()
		for _, t := range s.tenants {
			total += t.waiting + t.running
		}
		n = s.keda.Step(now, s.target, float64(total))
	case scalerCore:
		d := s.core.Step(s.observe())
		n = d.PoolReplicas
		s.noteForecast(d)
		if d.Policy != nil {
			s.schedule(s.now+s.st.PolicyDelay, &event{kind: evPolicy, pol: d.Policy})
		}
	}
	if n > s.target {
		s.actions = append(s.actions, s.now)
	}
	s.scaleTo(n)
	s.resetWindow()
	s.dispatch()
}

// resetWindow starts a new observation window.
func (s *simulator) resetWindow() {
	s.lastTick = s.now
	s.occSum, s.occN = 0, 0
	for _, t := range s.tenants {
		t.resetWindow()
	}
}

// noteForecast counts cycles in which the forecast raised a tenant's demand.
func (s *simulator) noteForecast(d control.Decision) {
	for _, r := range d.Records {
		if r.Tenant != control.PoolRecordTenant && r.Inputs.ForecastDemand > r.Inputs.ReactiveDemand {
			s.diag.ForecastRaisedCycles[r.Tenant]++
		}
	}
}

// applyPolicy installs a policy that reached the gateway, recording its
// propagation latency and, when it newly sheds the noisy tenant, a
// protective action.
func (s *simulator) applyPolicy(p *policy.Policy) {
	if s.sched == nil || !s.sched.SetPolicy(*p) {
		return
	}
	s.policyLat = append(s.policyLat, s.now-offset(p.IssuedAt))
	shed := s.roles.Noisy != "" && p.Tenants[s.roles.Noisy].Shed
	if shed && !s.noisyShed {
		s.actions = append(s.actions, s.now)
	}
	s.noisyShed = shed
	s.dispatch()
}
