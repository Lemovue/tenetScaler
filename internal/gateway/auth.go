package gateway

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"tenantscaler/internal/model"
)

// MinKeyLength is the shortest API key the gateway accepts, so a key cannot
// be guessed by trying a few values.
const MinKeyLength = 16

// TenantHeaderUsed names, in every answer to a keyed request, the tenant
// the key resolved to (a tenant header is ignored when keys are set).
const TenantHeaderUsed = "X-TenantScaler-Tenant"

// KeyGrant is what an API key entitles its holder to: requests as Tenant,
// at priority MaxPriority at most (decision Q16).
type KeyGrant struct {
	Tenant      string
	MaxPriority model.Priority
}

// ParseTenantKeys parses the TS_TENANT_KEYS form "A:standard=key1,B=key2"
// into a map from API key to grant: tenant, then optionally ":" and the
// highest priority the key may use (critical, standard, sheddable or an
// integer; default critical, i.e. no cap), then "=" and the key. Keys must
// be at least MinKeyLength characters and distinct; a tenant may have
// several keys. An empty string gives nil (no keys).
func ParseTenantKeys(s string) (map[string]KeyGrant, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	keys := map[string]KeyGrant{}
	for _, pair := range strings.Split(s, ",") {
		who, key, ok := strings.Cut(strings.TrimSpace(pair), "=")
		tenant, maxPrio, capped := strings.Cut(strings.TrimSpace(who), ":")
		tenant, key = strings.TrimSpace(tenant), strings.TrimSpace(key)
		switch {
		case !ok || tenant == "" || key == "":
			return nil, fmt.Errorf("tenant keys: want tenant[:maxPriority]=key pairs separated by commas, got %q", pair)
		case len(key) < MinKeyLength:
			return nil, fmt.Errorf("tenant keys: the key of tenant %s is shorter than %d characters", tenant, MinKeyLength)
		}
		grant := KeyGrant{Tenant: tenant, MaxPriority: model.PriorityCritical}
		if capped {
			p, err := model.ParsePriority(maxPrio)
			if err != nil {
				return nil, fmt.Errorf("tenant keys: tenant %s: %w", tenant, err)
			}
			grant.MaxPriority = p
		}
		if _, dup := keys[key]; dup {
			return nil, fmt.Errorf("tenant keys: tenant %s reuses another tenant's key", tenant)
		}
		keys[key] = grant
	}
	return keys, nil
}

// validateKeys checks the configured keys against the known tenants.
func validateKeys(cfg Config, tenants map[string]model.TenantSpec) error {
	for key, g := range cfg.TenantKeys {
		if _, ok := tenants[g.Tenant]; !ok {
			return fmt.Errorf("gateway: an API key is configured for unknown tenant %q", g.Tenant)
		}
		if len(key) < MinKeyLength {
			return fmt.Errorf("gateway: the API key of tenant %s is shorter than %d characters", g.Tenant, MinKeyLength)
		}
		if cfg.AdminKey != "" && key == cfg.AdminKey {
			return fmt.Errorf("gateway: tenant %s's API key equals the admin key", g.Tenant)
		}
	}
	if cfg.AdminKey != "" && len(cfg.AdminKey) < MinKeyLength {
		return fmt.Errorf("gateway: the admin key is shorter than %d characters", MinKeyLength)
	}
	return nil
}

// bearer returns the token of an "Authorization: Bearer <token>" header.
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// requestTenant returns the tenant of a completion request and the highest
// priority it may use. Without tenant keys the tenant is the tenant header
// and the priority is not capped. With keys the API key decides both (401
// when it is missing or unknown) and the tenant header is ignored: the key
// wins (decision Q16), and TenantHeaderUsed tells the client which tenant
// it was served as.
func (g *Gateway) requestTenant(w http.ResponseWriter, r *http.Request) (string, model.Priority, bool) {
	if len(g.cfg.TenantKeys) == 0 {
		return tenantOf(r), model.PriorityCritical, true
	}
	var grant KeyGrant
	token := []byte(bearer(r))
	for key, kg := range g.cfg.TenantKeys {
		if subtle.ConstantTimeCompare([]byte(key), token) == 1 {
			grant = kg
		}
	}
	if grant.Tenant == "" {
		unauthorized(w, "missing or unknown API key: send Authorization: Bearer <your tenant key>")
		return "", 0, false
	}
	w.Header().Set(TenantHeaderUsed, grant.Tenant)
	return grant.Tenant, grant.MaxPriority, true
}

// adminOnly requires the admin key on h when Config.AdminKey is set.
func (g *Gateway) adminOnly(h http.Handler) http.Handler {
	if g.cfg.AdminKey == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(g.cfg.AdminKey)) != 1 {
			unauthorized(w, "this endpoint needs the admin key: send Authorization: Bearer <admin key>")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// PublicHandler serves only what other machines need for the live demo:
// POST /v1/completions (tenant keys required) and GET /healthz. The full
// Handler, with /policy, /stats and /metrics, stays on the loopback
// listener. It answers 503 to completions when no tenant keys are set.
func (g *Gateway) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		if len(g.cfg.TenantKeys) == 0 {
			writeError(w, http.StatusServiceUnavailable, apiError{Type: "unavailable",
				Message: "this public endpoint needs tenant API keys ($TS_TENANT_KEYS)"})
			return
		}
		g.handleCompletions(w, r)
	})
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return mux
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="tenantscaler"`)
	writeError(w, http.StatusUnauthorized, apiError{Type: "authentication_error", Message: msg})
}
