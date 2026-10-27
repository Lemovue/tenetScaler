package controller

import (
	"context"
	"errors"
)

// VersionSource reports the newest gateway policy version already in use:
// HTTPPublisher reads it from the gateways, KubeTenants from the
// TenantScaler statuses a previous controller wrote.
type VersionSource interface {
	PolicyVersion(ctx context.Context) (int64, error)
}

// SeedPolicyVersion raises the core's policy version to the newest one the
// sources report, so a restarted controller's first policy is newer than
// the one the gateways hold and is applied instead of ignored. Call it
// before Run. Sources that fail are skipped and their errors returned
// joined; that is not fatal, since a cycle whose policy a gateway ignores as
// stale also raises the version (one cycle later).
func (r *Runner) SeedPolicyVersion(ctx context.Context, sources ...VersionSource) error {
	if r.Core == nil {
		return errors.New("seed policy version: Core is nil")
	}
	var newest int64
	var errs []error
	for _, s := range sources {
		v, err := s.PolicyVersion(ctx)
		if err != nil {
			errs = append(errs, err)
		}
		newest = max(newest, v)
	}
	r.cycleMu.Lock()
	defer r.cycleMu.Unlock()
	if r.adoptPolicyVersion(newest) {
		r.logger().Info("continuing the policy versions in use", "inUse", newest, "nextVersion", newest+1)
	}
	return errors.Join(errs...)
}

// adoptPolicyVersion raises the core's policy version to v when v is newer
// and reports whether it did. The caller holds cycleMu.
func (r *Runner) adoptPolicyVersion(v int64) bool {
	s := r.Core.State()
	if v <= s.PolicyVersion {
		return false
	}
	s.PolicyVersion = v
	r.Core.Restore(s)
	return true
}
