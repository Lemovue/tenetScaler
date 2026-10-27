package experiment

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
)

func TestProfileFindsCapacity(t *testing.T) {
	cfg := config.Default()
	res, err := Profile(cfg, ProfileOptions{Duration: 120 * time.Second, Seeds: 1, BaseSeed: 1, Precision: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	if !(res.CapacityTokensPerSec > 0) || !(res.MeanWeightedTokens > 300 && res.MeanWeightedTokens < 900) {
		t.Fatalf("C = %v, mean request = %v weighted tokens", res.CapacityTokensPerSec, res.MeanWeightedTokens)
	}
	// C is a probed load that met the SLO, and every probe above the
	// lowest failing one failed (the search brackets the boundary).
	lowestFail := 0.0
	met := false
	for _, p := range res.Points {
		if p.WeightedTokensPerSec == res.CapacityTokensPerSec && p.Meets {
			met = true
		}
		if !p.Meets && (lowestFail == 0 || p.WeightedTokensPerSec < lowestFail) {
			lowestFail = p.WeightedTokensPerSec
		}
	}
	if !met || !(lowestFail > res.CapacityTokensPerSec) || (lowestFail-res.CapacityTokensPerSec) > 0.05*lowestFail+1e-9 {
		t.Fatalf("C = %v, lowest failing probe %v, points %+v", res.CapacityTokensPerSec, lowestFail, res.Points)
	}
	var out bytes.Buffer
	if err := res.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "SIMULATED") || !strings.Contains(s, "C (simulated) =") {
		t.Fatalf("profile text:\n%s", s)
	}
}

func TestStrictestSLO(t *testing.T) {
	got := strictestSLO([]model.TenantSpec{
		{SLO: model.SLO{TTFTSeconds: 2, TPOTMillis: 50, TargetFraction: 0.9}},
		{SLO: model.SLO{TTFTSeconds: 1, TPOTMillis: 100, TargetFraction: 0.99}},
	})
	if got != (model.SLO{TTFTSeconds: 1, TPOTMillis: 50, TargetFraction: 0.99}) {
		t.Fatalf("strictest = %+v", got)
	}
}
