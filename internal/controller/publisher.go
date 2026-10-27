package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"tenantscaler/internal/config"
	"tenantscaler/internal/policy"
)

// HTTPPublisher POSTs the policy as JSON to {url}/policy of every gateway,
// concurrently. A transport error, a non-2xx answer or a policy the gateway
// did not apply is an error; the gateways that applied it keep it.
type HTTPPublisher struct {
	URLs []string
	// HTTP is the client to use; nil means a client with a 5 s timeout.
	HTTP *http.Client
}

// NewPublisher returns the publisher for cfg's gateways, or nil when gateway
// coupling is disabled (ablation). Coupling needs at least one gateway URL.
func NewPublisher(cfg config.Config, gateways []string) (*HTTPPublisher, error) {
	if cfg.Controller.DisableGatewayCoupling {
		return nil, nil
	}
	if len(gateways) == 0 {
		return nil, errors.New("gateway coupling needs at least one gateway URL")
	}
	return &HTTPPublisher{URLs: gateways}, nil
}

// StaleVersionError reports a gateway that kept a newer policy than the one
// published and ignored it. This happens after a controller restart, when
// the new controller's versions begin again at 1. The Runner then continues
// numbering above GatewayVersion, so its next policy is applied.
type StaleVersionError struct {
	// URL is the gateway's policy endpoint.
	URL string
	// Version is the version that was published.
	Version int64
	// GatewayVersion is the newer version the gateway keeps in force.
	GatewayVersion int64
}

// Error describes the ignored policy.
func (e *StaleVersionError) Error() string {
	return fmt.Sprintf("POST %s: the gateway ignored policy v%d and keeps the newer v%d", e.URL, e.Version, e.GatewayVersion)
}

// policyReply is the gateway's answer to POST /policy.
type policyReply struct {
	// Applied is false when the gateway kept a newer policy; nil means the
	// answer is not a TenantScaler gateway's.
	Applied *bool `json:"applied"`
	// Version is the policy version in force after the call.
	Version int64 `json:"version"`
}

// Publish sends pol to every gateway and joins the errors of those that
// failed or did not apply it. A gateway holding a newer version yields a
// *StaleVersionError in the joined error.
func (p HTTPPublisher) Publish(ctx context.Context, pol policy.Policy) error {
	if len(p.URLs) == 0 {
		return errors.New("publish policy: no gateway URLs")
	}
	body, err := json.Marshal(pol)
	if err != nil {
		return fmt.Errorf("publish policy v%d: encode: %w", pol.Version, err)
	}
	errs := make([]error, len(p.URLs))
	var wg sync.WaitGroup
	for i, u := range p.URLs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			url := strings.TrimRight(u, "/") + "/policy"
			var reply policyReply
			if err := doJSON(ctx, p.HTTP, http.MethodPost, url, body, &reply); err != nil {
				errs[i] = fmt.Errorf("publish policy v%d: %w", pol.Version, err)
				return
			}
			errs[i] = reply.check(url, pol.Version)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// check turns a reply that did not apply the published version into an
// error.
func (r policyReply) check(url string, version int64) error {
	switch {
	case r.Applied == nil:
		return fmt.Errorf("publish policy v%d: POST %s: the answer has no \"applied\" field (is this a TenantScaler gateway?)", version, url)
	case *r.Applied:
		return nil
	case r.Version > version:
		return &StaleVersionError{URL: url, Version: version, GatewayVersion: r.Version}
	default:
		return fmt.Errorf("publish policy v%d: POST %s: not applied (the gateway keeps v%d)", version, url, r.Version)
	}
}

// PolicyVersion returns the newest policy version in force on any gateway
// (GET {url}/policy). It returns the newest version of the gateways that
// answered, and the joined errors of those that did not.
func (p HTTPPublisher) PolicyVersion(ctx context.Context) (int64, error) {
	if len(p.URLs) == 0 {
		return 0, errors.New("read gateway policy version: no gateway URLs")
	}
	var newest int64
	var errs []error
	for _, u := range p.URLs {
		var cur struct {
			Version int64 `json:"version"`
		}
		url := strings.TrimRight(u, "/") + "/policy"
		if err := doJSON(ctx, p.HTTP, http.MethodGet, url, nil, &cur); err != nil {
			errs = append(errs, fmt.Errorf("read gateway policy version: %w", err))
			continue
		}
		newest = max(newest, cur.Version)
	}
	return newest, errors.Join(errs...)
}

// staleGatewayVersion returns the newest GatewayVersion of the
// *StaleVersionErrors in err's tree, or 0 if there is none.
func staleGatewayVersion(err error) int64 {
	var v int64
	if s, ok := err.(*StaleVersionError); ok {
		v = s.GatewayVersion
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			v = max(v, staleGatewayVersion(e))
		}
	case interface{ Unwrap() error }:
		v = max(v, staleGatewayVersion(u.Unwrap()))
	}
	return v
}
