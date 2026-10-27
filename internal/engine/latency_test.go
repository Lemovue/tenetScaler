package engine

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func testParams() Params {
	return Params{
		PrefillOverhead:     40 * time.Millisecond,
		PrefillPerToken:     time.Millisecond,
		InterTokenLatency:   20 * time.Millisecond,
		MaxNumSeqs:          5,
		TimeFactorUnderLoad: 3,
	}
}

func TestLoadFactor(t *testing.T) {
	p := testParams()
	single := p
	single.MaxNumSeqs = 1
	flat := p
	flat.TimeFactorUnderLoad = 1
	tests := []struct {
		name    string
		p       Params
		running int
		want    float64
	}{
		{"idle clamps to 1", p, 0, 1},
		{"one request", p, 1, 1},
		{"halfway", p, 3, 2},
		{"full", p, 5, 3},
		{"over full clamps to F", p, 9, 3},
		{"MaxNumSeqs 1", single, 4, 1},
		{"factor 1", flat, 5, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.LoadFactor(tc.running); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("LoadFactor(%d) = %v, want %v", tc.running, got, tc.want)
			}
		})
	}
}

func TestPrefillTime(t *testing.T) {
	p := testParams()
	tests := []struct {
		name        string
		in, running int
		want        time.Duration
	}{
		{"no input at load 1", 0, 1, 40 * time.Millisecond},
		{"negative input is zero", -3, 1, 40 * time.Millisecond},
		{"100 tokens at load 1", 100, 1, 140 * time.Millisecond},
		{"100 tokens at load 2", 100, 3, 280 * time.Millisecond},
		{"10 tokens at full load", 10, 5, 150 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.PrefillTime(tc.in, tc.running); got != tc.want {
				t.Fatalf("PrefillTime(%d, %d) = %v, want %v", tc.in, tc.running, got, tc.want)
			}
		})
	}
}

func TestTokenInterval(t *testing.T) {
	p := testParams()
	tests := []struct {
		running int
		want    time.Duration
	}{
		{0, 20 * time.Millisecond},
		{1, 20 * time.Millisecond},
		{2, 30 * time.Millisecond},
		{5, 60 * time.Millisecond},
		{50, 60 * time.Millisecond},
	}
	for _, tc := range tests {
		if got := p.TokenInterval(tc.running); got != tc.want {
			t.Errorf("TokenInterval(%d) = %v, want %v", tc.running, got, tc.want)
		}
	}
}

func TestJitterBoundsAndDeterminism(t *testing.T) {
	d := 100 * time.Millisecond
	a, b := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	varied := false
	for i := 0; i < 2000; i++ {
		ja := Jitter(d, 0.3, a)
		if jb := Jitter(d, 0.3, b); ja != jb {
			t.Fatalf("same seed gave %v and %v", ja, jb)
		}
		if ja < d/2 || ja > d*3/2 {
			t.Fatalf("Jitter = %v, outside [%v, %v]", ja, d/2, d*3/2)
		}
		if ja != d {
			varied = true
		}
	}
	if !varied {
		t.Fatal("jitter never changed the duration")
	}
}

func TestJitterDisabled(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tests := []struct {
		name string
		d    time.Duration
		frac float64
		rng  *rand.Rand
	}{
		{"zero fraction", time.Second, 0, rng},
		{"nil rng", time.Second, 0.1, nil},
		{"zero duration", 0, 0.1, rng},
		{"negative duration", -time.Second, 0.1, rng},
	}
	for _, tc := range tests {
		if got := Jitter(tc.d, tc.frac, tc.rng); got != tc.d {
			t.Errorf("%s: Jitter = %v, want %v", tc.name, got, tc.d)
		}
	}
}

func TestCountTokens(t *testing.T) {
	tests := []struct {
		prompt string
		want   int
	}{
		{"", 1},
		{"   \n\t", 1},
		{"hello", 1},
		{"hello world", 2},
		{"  the quick\tbrown\nfox  ", 4},
	}
	for _, tc := range tests {
		if got := CountTokens(tc.prompt); got != tc.want {
			t.Errorf("CountTokens(%q) = %d, want %d", tc.prompt, got, tc.want)
		}
	}
}
