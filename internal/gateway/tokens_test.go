package gateway

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// With a token counter the gateway's demand uses the replica's tokenizer,
// and a failing counter falls back to words and is counted.
func TestTokenCounter(t *testing.T) {
	up := newFakeUpstream(t, nil)
	var fail bool
	counter := func(_ context.Context, endpoint, prompt string) (int, error) {
		if endpoint != up.URL() {
			t.Errorf("tokenizer asked %q, want the ready replica", endpoint)
		}
		if fail {
			return 0, errors.New("tokenizer down")
		}
		return 3 * len(strings.Fields(prompt)), nil // a tokenizer with 3 tokens per word
	}
	g, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL()}, TokenCounter: counter})
	weighted := func() float64 { // A's weighted tokens in the stats window
		st := g.Stats(0)
		for _, o := range st.Tenants {
			if o.Tenant == "A" {
				return o.ArrivalWeightedTokensPerSec * st.WindowSeconds
			}
		}
		t.Fatal("no stats for A")
		return 0
	}
	waitFor(t, "the endpoint", 5*time.Second, func() bool { return g.Stats(0).Endpoints == 1 })
	body := completionBody("w w", 1, false) // 2 words -> 6 tokens; weighted = 6 + 2 x 1
	post(t, base, "A", "", body)
	if got := weighted(); math.Abs(got-8) > 1e-6 {
		t.Fatalf("A's weighted tokens %v, want 8 (6 tokenizer tokens + 2 x 1 output)", got)
	}
	fail = true
	post(t, base, "A", "", body) // 2 words + 2 x 1
	if got := weighted(); math.Abs(got-12) > 1e-6 {
		t.Fatalf("A's weighted tokens %v, want 12 after a word-count fallback", got)
	}
	if n := g.TokenizerFallbacks(); n != 1 {
		t.Fatalf("fallbacks = %d, want 1", n)
	}
}
