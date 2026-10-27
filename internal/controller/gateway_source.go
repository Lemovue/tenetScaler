package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"tenantscaler/internal/model"
)

// GatewaySource reads windowed per-tenant statistics from one or more
// gateways (GET {url}/stats?window=<Window>) and merges them. If any gateway
// cannot be read the whole observation fails, so the controller fails static
// instead of acting on a partial view of demand.
type GatewaySource struct {
	URLs []string
	// Window is the statistics window; zero means the gateway's default.
	Window time.Duration
	// HTTP is the client to use; nil means a client with a 5 s timeout.
	HTTP *http.Client
}

// gatewayStats is the part of the gateway's /stats answer the controller
// needs: the per-tenant observations (the Runner stamps the time with its
// own clock). The gateway package is not imported so the two stay
// decoupled.
type gatewayStats struct {
	Tenants []model.TenantObservation `json:"tenants"`
}

// Observe fetches /stats from every gateway and merges the tenants.
func (s GatewaySource) Observe(ctx context.Context) (model.Observation, error) {
	if len(s.URLs) == 0 {
		return model.Observation{}, errors.New("gateway source: no gateway URLs")
	}
	all := make([][]model.TenantObservation, 0, len(s.URLs))
	for _, u := range s.URLs {
		target := strings.TrimRight(u, "/") + "/stats"
		if s.Window > 0 {
			target += "?window=" + url.QueryEscape(s.Window.String())
		}
		var st gatewayStats
		if err := doJSON(ctx, s.HTTP, http.MethodGet, target, nil, &st); err != nil {
			return model.Observation{}, fmt.Errorf("gateway source: %w", err)
		}
		all = append(all, st.Tenants)
	}
	return model.Observation{OK: true, Tenants: MergeTenants(all...)}, nil
}

// MergeTenants combines per-tenant observations from several gateways that
// share the tenants' traffic: rates, queue, in-flight, completed and shed
// add up; SLO attainment is the completed-weighted mean of the gateways that
// have SLO data (the plain mean if none completed anything); p99 TTFT is the
// maximum, a conservative upper bound. The result is sorted by tenant name.
func MergeTenants(groups ...[]model.TenantObservation) []model.TenantObservation {
	type acc struct {
		obs          model.TenantObservation
		attSum       float64 // sum of attainment * completed
		attPlain     float64 // sum of attainment, for the zero-completed fallback
		sloCompleted int     // completed requests of the groups with SLO data
		sloGroups    int
	}
	byName := map[string]*acc{}
	for _, g := range groups {
		for _, o := range g {
			a := byName[o.Tenant]
			if a == nil {
				a = &acc{obs: model.TenantObservation{Tenant: o.Tenant}}
				byName[o.Tenant] = a
			}
			m := &a.obs
			m.ArrivalWeightedTokensPerSec += o.ArrivalWeightedTokensPerSec
			m.AdmittedWeightedTokensPerSec += o.AdmittedWeightedTokensPerSec
			m.ArrivalRequestsPerSec += o.ArrivalRequestsPerSec
			m.QueueLength += o.QueueLength
			m.InFlight += o.InFlight
			m.Completed += o.Completed
			m.Shed += o.Shed
			if o.HasSLOData {
				m.HasSLOData = true
				m.P99TTFTSeconds = max(m.P99TTFTSeconds, o.P99TTFTSeconds)
				a.attSum += o.SLOAttainment * float64(o.Completed)
				a.attPlain += o.SLOAttainment
				a.sloCompleted += o.Completed
				a.sloGroups++
			}
		}
	}
	out := make([]model.TenantObservation, 0, len(byName))
	for _, a := range byName {
		m := a.obs
		if m.HasSLOData {
			if a.sloCompleted > 0 {
				m.SLOAttainment = a.attSum / float64(a.sloCompleted)
			} else {
				m.SLOAttainment = a.attPlain / float64(a.sloGroups)
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}
