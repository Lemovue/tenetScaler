package model

import (
	"testing"
	"time"
)

func TestParsePriority(t *testing.T) {
	cases := map[string]Priority{
		"": PriorityStandard, "standard": PriorityStandard, "CRITICAL": PriorityCritical,
		"sheddable": PrioritySheddable, "-3": -3, "5": 5,
	}
	for in, want := range cases {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Errorf("ParsePriority(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParsePriority("urgent"); err == nil {
		t.Error("expected error for unknown class")
	}
}

func TestPriorityClass(t *testing.T) {
	if PrioritySheddable.Class() != "sheddable" || !PrioritySheddable.Sheddable() {
		t.Error("sheddable misclassified")
	}
	if PriorityCritical.Class() != "critical" || !PriorityCritical.Critical() {
		t.Error("critical misclassified")
	}
	if PriorityStandard.Class() != "standard" || PriorityStandard.Sheddable() || PriorityStandard.Critical() {
		t.Error("standard misclassified")
	}
}

func TestWeightedTokens(t *testing.T) {
	if got := WeightedTokens(100, 50); got != 200 {
		t.Fatalf("WeightedTokens(100,50) = %v, want 200", got)
	}
}

func TestSLOMet(t *testing.T) {
	s := SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	if !s.Met(time.Second, 50*time.Millisecond, 10) {
		t.Error("fast request should meet SLO")
	}
	if s.Met(3*time.Second, 50*time.Millisecond, 10) {
		t.Error("slow TTFT should miss SLO")
	}
	if s.Met(time.Second, 200*time.Millisecond, 10) {
		t.Error("slow TPOT should miss SLO")
	}
	if !s.Met(time.Second, 200*time.Millisecond, 1) {
		t.Error("single-token request only needs TTFT")
	}
}
