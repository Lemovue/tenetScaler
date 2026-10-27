package sim

import (
	"slices"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/policy"
)

// initGateway builds the gateway scheduler(s) of the setup's mode.
func (s *simulator) initGateway() {
	switch s.spec.gateway {
	case GatewayFair, GatewayRoundRobin:
		mode := fairq.ModeFair
		if s.spec.gateway == GatewayRoundRobin {
			mode = fairq.ModeRoundRobin
		}
		s.sched = fairq.New(fairq.Config{Mode: mode, Seed: s.o.Seed})
		// The live gateway's fail-safe policy before the controller speaks.
		s.sched.SetPolicy(policy.Equal(s.cfg.TenantNames()))
	case GatewayPartition:
		s.partSched = map[string]*fairq.Scheduler{}
		for _, t := range s.tenants {
			s.partSched[t.name] = fairq.New(fairq.Config{Mode: fairq.ModeFCFS, Seed: s.o.Seed})
		}
	}
}

// endpointsChanged reacts to the ready set changing: partition mode
// re-deals replicas to tenants, passthrough flushes held requests.
func (s *simulator) endpointsChanged() {
	switch s.spec.gateway {
	case GatewayPartition:
		s.assignPartitions()
	case GatewayPassthrough:
		s.flushPending()
	}
}

// assignPartitions deals the ready replicas (by id) to tenants: tenant i
// owns a contiguous slice of GuaranteedReplicas[i] replicas, and leftovers
// are dealt round robin in tenant order.
func (s *simulator) assignPartitions() {
	eps := s.readyReps()
	pos := 0
	for _, t := range s.cfg.Tenants {
		for k := 0; k < t.GuaranteedReplicas && pos < len(eps); k++ {
			eps[pos].owner = t.Name
			pos++
		}
	}
	for k := 0; pos < len(eps); k++ {
		eps[pos].owner = s.cfg.Tenants[k%len(s.cfg.Tenants)].Name
		pos++
	}
}

// arrive handles a request reaching the gateway.
func (s *simulator) arrive(req *request) {
	req.ts.noteArrival(req.spec)
	switch s.spec.gateway {
	case GatewayPassthrough:
		req.setState(stGateway)
		s.pending = append(s.pending, req)
		s.flushPending()
		s.armTimeout(req)
	case GatewayPartition:
		s.admit(s.partSched[req.ts.name], req, false)
	default:
		saturated := leastLoaded(s.readyReps(), s.st.SlotsPerReplica) == nil
		s.admit(s.sched, req, saturated)
	}
}

// admit offers req to a fairq scheduler: a rejection sheds it, an
// admission queues it and triggers dispatch.
func (s *simulator) admit(sched *fairq.Scheduler, req *request, saturated bool) {
	arrival := s.clock()
	fq := &fairq.Request{
		ID: uint64(req.id), Tenant: req.ts.name, Priority: req.spec.Priority,
		InputTokens: req.spec.InputTokens, MaxOutputTokens: req.spec.OutputTokens,
		Arrival: arrival, Deadline: arrival.Add(req.ts.slo.TTFT()),
	}
	s.chargeTokens() // the counter lift reads the counters
	v := sched.Admit(fq, arrival, saturated)
	if !v.Admitted {
		s.shed(req, v.Reason)
		return
	}
	req.fq, req.sched = fq, sched
	req.setState(stGateway)
	s.dispatch()
	s.armTimeout(req)
}

// armTimeout schedules the dispatch timeout of a request still waiting at
// the gateway, as the live gateway starts its timer on admission.
func (s *simulator) armTimeout(req *request) {
	if req.state == stGateway {
		s.schedule(s.now+s.st.DispatchTimeout, &event{kind: evTimeout, req: req})
	}
}

// onTimeout fails a request that is still waiting at the gateway when its
// DispatchTimeout expires: the live gateway withdraws it from the queue
// and answers 503, which the evaluation records as Failed.
func (s *simulator) onTimeout(req *request) {
	if req.state != stGateway {
		return
	}
	if req.sched != nil {
		req.sched.Cancel(req.fq)
	} else {
		s.pending = slices.DeleteFunc(s.pending, func(r *request) bool { return r == req })
	}
	req.setState(stTimedOut)
	s.resolved++
}

// chargeTokens reports to the fair scheduler the output tokens each
// dispatched request has streamed so far, as the live gateway does on
// every SSE chunk, so a tenant's counter grows during decode once its
// tokens exceed the estimate charged at dispatch. The scheduler reads its
// counters only in Admit (lift), Next and Snapshot, and the simulator
// calls this before each of them, so the counters match the live
// gateway's at every instant they matter.
func (s *simulator) chargeTokens() {
	for _, req := range s.charging {
		req.sched.OnOutputTokens(req.fq, req.produced(s.now))
	}
}

// dispatch moves queued requests to replicas while the gateway can.
func (s *simulator) dispatch() {
	switch s.spec.gateway {
	case GatewayPassthrough:
		s.flushPending()
	case GatewayPartition:
		eps := s.readyReps()
		for _, t := range s.tenants {
			var own []*replica
			for _, r := range eps {
				if r.owner == t.name {
					own = append(own, r)
				}
			}
			s.dispatchFrom(s.partSched[t.name], own)
		}
	default:
		s.dispatchFrom(s.sched, s.readyReps())
	}
}

// dispatchFrom takes requests from sched while one of eps has a free
// dispatch-window slot, sending each to the least-loaded replica.
func (s *simulator) dispatchFrom(sched *fairq.Scheduler, eps []*replica) {
	if sched.Len() > 0 {
		s.chargeTokens() // Next picks by counter
	}
	for sched.Len() > 0 {
		r := leastLoaded(eps, s.st.SlotsPerReplica)
		if r == nil {
			return
		}
		free := 0
		for _, e := range eps {
			free += max(0, s.st.SlotsPerReplica-e.assigned)
		}
		fq := sched.NextWithin(s.clock(), free)
		if fq == nil {
			return // the free slots are reserved for other tenants' shares
		}
		req := s.reqs[fq.ID]
		s.accept(r, req)
		if s.spec.gateway == GatewayFair {
			s.charging = append(s.charging, req)
		}
	}
}

// flushPending forwards held passthrough requests round robin over the
// ready replicas; with none ready they keep waiting at the gateway.
func (s *simulator) flushPending() {
	if len(s.pending) == 0 {
		return
	}
	eps := s.readyReps()
	if len(eps) == 0 {
		return
	}
	for _, req := range s.pending {
		r := eps[s.rrNext%len(eps)]
		s.rrNext++
		s.accept(r, req)
	}
	s.pending = s.pending[:0]
}
