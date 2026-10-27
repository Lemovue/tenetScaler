package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenantscaler/internal/model"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestRejectsGuaranteesAboveCapacity(t *testing.T) {
	cfg := Default()
	cfg.Tenants[0].GuaranteedReplicas = 5 // 5+3+3 = 11 > 10
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "refusing to promise more than exists") {
		t.Fatalf("expected over-promise rejection, got %v", err)
	}
}

func TestRejectsOverflowingGuarantees(t *testing.T) {
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	huge := int(^uint(0)>>1)/2 + 1
	tenants := []model.TenantSpec{
		{Name: "a", GuaranteedReplicas: huge, MaxReplicas: huge, SLO: slo},
		{Name: "b", GuaranteedReplicas: huge, MaxReplicas: huge, SLO: slo},
	}
	err := ValidateTenants(tenants, 10)
	if err == nil || !strings.Contains(err.Error(), "refusing to promise more than exists") {
		t.Fatalf("expected over-promise error without overflow, got %v", err)
	}
}

func TestValidateTenantsRejectsBadSpecs(t *testing.T) {
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	cases := map[string][]model.TenantSpec{
		"empty":     nil,
		"duplicate": {{Name: "a", MaxReplicas: 1, SLO: slo}, {Name: "a", MaxReplicas: 1, SLO: slo}},
		"bad name":  {{Name: "has space", MaxReplicas: 1, SLO: slo}},
		"max<guar":  {{Name: "a", GuaranteedReplicas: 3, MaxReplicas: 2, SLO: slo}},
		"neg guar":  {{Name: "a", GuaranteedReplicas: -1, MaxReplicas: 2, SLO: slo}},
		"zero slo":  {{Name: "a", MaxReplicas: 2}},
		"target>1":  {{Name: "a", MaxReplicas: 2, SLO: model.SLO{TTFTSeconds: 1, TPOTMillis: 1, TargetFraction: 1.5}}},
	}
	for name, tenants := range cases {
		if err := ValidateTenants(tenants, 10); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadOverlaysDefaultsAndRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("pool:\n  name: p\n  capacityReplicas: 12\n  minReplicas: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Pool.CapacityReplicas != 12 || len(cfg.Tenants) != 3 {
		t.Fatalf("overlay not applied: %+v", cfg.Pool)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("pool:\n  capacityReplica: 12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestBackendValidation(t *testing.T) {
	ok := Default().Backend
	ok.Kind = BackendLlama
	if err := ok.Validate(4); err != nil {
		t.Fatalf("default llama backend should validate: %v", err)
	}
	bad := map[string]func(*BackendSpec){
		"kind":       func(b *BackendSpec) { b.Kind = "gpu-magic" },
		"threads":    func(b *BackendSpec) { b.Llama.Threads = 0 },
		"parallel":   func(b *BackendSpec) { b.Llama.Parallel = 0 },
		"ctx":        func(b *BackendSpec) { b.Llama.CtxPerSlot = 64 },
		"ngl":        func(b *BackendSpec) { b.Llama.GPULayers = -1 },
		"timeout":    func(b *BackendSpec) { b.Llama.ReadyTimeoutSeconds = 0 },
		"few masks":  func(b *BackendSpec) { b.Llama.CPUMasks = []string{"3"} },
		"bad mask":   func(b *BackendSpec) { b.Llama.CPUMasks = []string{"3", "c", "zz", "c0"} },
		"empty mask": func(b *BackendSpec) { b.Llama.CPUMasks = []string{"3", "c", "", "c0"} },
	}
	for name, mutate := range bad {
		b := ok
		b.Llama.CPUMasks = nil
		mutate(&b)
		if err := b.Validate(4); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	sim := Default().Backend
	sim.Llama.Threads = 0 // ignored for the simulated backend
	if err := sim.Validate(4); err != nil {
		t.Fatalf("simulated backend should ignore llama settings: %v", err)
	}
}

func TestSlotsPerReplica(t *testing.T) {
	cfg := Default()
	if got := cfg.SlotsPerReplica(); got != cfg.Engine.MaxNumSeqs {
		t.Fatalf("simulated default slots = %d, want engine maxNumSeqs %d", got, cfg.Engine.MaxNumSeqs)
	}
	cfg.Backend.Kind = BackendLlama
	if got := cfg.SlotsPerReplica(); got != cfg.Backend.Llama.Parallel {
		t.Fatalf("llama slots = %d, want parallel %d", got, cfg.Backend.Llama.Parallel)
	}
	cfg.Gateway.SlotsPerReplica = 3
	if got := cfg.SlotsPerReplica(); got != 3 {
		t.Fatalf("explicit gateway slots = %d, want 3", got)
	}
}

func TestRealConfigFileIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config", "real.yaml"))
	if err != nil {
		t.Fatalf("config/real.yaml: %v", err)
	}
	if cfg.Backend.Kind != BackendLlama || cfg.Pool.CapacityReplicas != 4 {
		t.Fatalf("real.yaml should select the llama backend with a 4-replica budget, got %q / %d", cfg.Backend.Kind, cfg.Pool.CapacityReplicas)
	}
}

func TestEngineSpecRoundTrip(t *testing.T) {
	p, err := Default().Engine.Params()
	if err != nil {
		t.Fatal(err)
	}
	if got := EngineFromParams(p); got != Default().Engine {
		t.Fatalf("round trip changed engine spec: %+v vs %+v", got, Default().Engine)
	}
	if _, err := (EngineSpec{PrefillOverhead: "x"}).Params(); err == nil {
		t.Fatal("expected a duration parse error")
	}
}
