package realrun

import (
	"errors"
	"fmt"
	"log/slog"

	"tenantscaler/internal/config"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/topology"
)

// pinServices pins this process (the gateway, controller and load
// generator of a run) to the CPUs the budget's replicas do not use and
// returns the description recorded with the results (decision Q13).
// Simulated backends are not pinned.
func pinServices(cfg config.Config, logger *slog.Logger) (string, error) {
	if cfg.Backend.Kind != config.BackendLlama {
		return "", nil
	}
	p, err := llama.PinServices(cfg.Backend.Llama, cfg.Pool.CapacityReplicas)
	switch {
	case errors.Is(err, topology.ErrUnsupported):
		logger.Warn("services not pinned: CPU pinning is implemented on Windows only")
		return "services not pinned (CPU pinning is implemented on Windows only)", nil
	case err != nil:
		return "", fmt.Errorf("pin services: %w", err)
	}
	logger.Info("CPU pinning", "pinning", p.String())
	return p.String(), nil
}
