package loadgen

import (
	"strings"
	"testing"

	"tenantscaler/internal/engine"
)

func TestVocabulary(t *testing.T) {
	if len(vocabulary) < 256 {
		t.Fatalf("vocabulary has %d words, want a few hundred", len(vocabulary))
	}
	seen := map[string]bool{}
	for _, w := range vocabulary {
		if seen[w] {
			t.Errorf("duplicate word %q", w)
		}
		seen[w] = true
		if strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Errorf("word %q is not plain lowercase letters", w)
		}
	}
}

func TestPromptLengthAndDeterminism(t *testing.T) {
	for _, n := range []int{-3, 0, 1, 2, 17, 600} {
		p := Prompt(5, 42, n)
		if got, want := engine.CountTokens(p), max(n, 1); got != want {
			t.Errorf("Prompt(n=%d) has %d words, want %d", n, got, want)
		}
		if Prompt(5, 42, n) != p {
			t.Errorf("Prompt(n=%d) is not deterministic", n)
		}
	}
	if Prompt(1, 9, 32) == Prompt(2, 9, 32) {
		t.Error("different seeds gave the same prompt")
	}
	if !strings.HasPrefix(Prompt(3, 9, 40), Prompt(3, 9, 10)) {
		t.Error("a longer prompt of the same request should extend the shorter one")
	}
}

func TestPromptsNeverShareTwoWordPrefix(t *testing.T) {
	const n = 30000 // well beyond the vocabulary size, below its square
	firstTwo := make(map[string]uint64, n)
	for i := range uint64(n) {
		w := strings.Fields(Prompt(11, i, 16))
		key := w[0] + " " + w[1]
		if j, dup := firstTwo[key]; dup {
			t.Fatalf("requests %d and %d both start with %q", j, i, key)
		}
		firstTwo[key] = i
	}
}
