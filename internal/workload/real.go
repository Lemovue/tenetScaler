package workload

// RealLengths returns the request length distributions of the real-replica
// scenarios (RealScenario, RealDemoScenario): short prompts (median 80
// words, at most 160) and short answers (median 48 tokens, at most 96), so
// that a run of a few minutes on CPU-pinned llama.cpp replicas holds
// hundreds of requests. Prompts are capped at 160 words because prefill
// is slow on two CPU threads: the prefill fit of results/real/profile.md
// (8.2 ms + 7.94 ms per word, from median TTFTs measured with 16- and
// 160-word prompts on an idle replica) extrapolates to about 2.0 s for a
// 256-word prompt, which alone would reach the 2 s TTFT SLO; at 160 words
// the measured median TTFT is 1.28 s, which leaves room for queueing.
func RealLengths() (in, out LengthDist) {
	return LengthDist{Median: 80, Sigma: 0.4, Min: 16, Max: 160},
		LengthDist{Median: 48, Sigma: 0.3, Min: 8, Max: 96}
}

// Rates of the real scenarios, sized for config/real.yaml (4 CPU-pinned
// llama.cpp replicas, 2 threads and 4 slots each) on the development
// machine (Intel Core i5-14450HX). Derivation:
//
//   - `tenantscaler profile -config config/real.yaml` (llama.cpp
//     b11898, Qwen2.5-0.5B-Instruct Q4_K_M, RealLengths, results/real/profile.md)
//     measured, with every replica pinned to its own P-core, C = 117
//     weighted tokens/s per replica while all 4 replicas are busy (prompt
//     words + 2 x output tokens; results/real/scaling.md; one replica alone
//     serves 136 at concurrency 4).
//   - RealLengths average 85.4 prompt words and 50.1 output tokens, i.e. 186
//     weighted tokens per request, so one replica serves about 117/186 = 0.63
//     requests/s and the full pool of 4 about 2.5 requests/s (467 weighted
//     tokens/s, measured).
//   - Base load: A 0.3 + B 0.2 (0.4 during its bursts, 0.27 on average) + C 0.2
//     = 0.77 requests/s on average, 0.9 at B's bursts, i.e. 143-167 weighted
//     tokens/s: 61-71% of 2 replicas, so the base load fits about 2 replicas.
//   - Spike: C at 3 requests/s makes 3.57 requests/s in total, about 663
//     weighted tokens/s = 142% of the full 4-replica pool, so even the whole
//     budget cannot serve everyone and a policy must choose; 70% of C's
//     requests are sheddable (batch mix).
//
// The rates are expected values; real_test.go checks the sizing on the
// traces of seeds 1-5 (the real runs' seeds) against the same C.
const (
	realRateA = 0.3 // A, steady
	realRateB = 0.2 // B outside its bursts
	realPeakB = 0.4 // B during its bursts
	realRateC = 0.2 // C outside its spike
	realPeakC = 3.0 // C during its spike
)

// RealScenario is the real-replica evaluation trace (240 s): A steady, B
// periodic (20 s bursts every 60 s), C spiking from 90 s to 170 s.
func RealScenario() Scenario {
	return realScenario("real", 240, 60, 20, 90, 80)
}

// RealDemoScenario is the shorter trace of the demo (120 s): the same
// tenants and rates as RealScenario, with B bursting 15 s every 40 s and C
// spiking from 45 s to 85 s.
func RealDemoScenario() Scenario {
	return realScenario("real-demo", 120, 40, 15, 45, 40)
}

// realScenario builds a real trace with the given timing in seconds: the
// duration, B's burst period and length (the first burst starts at one
// burst length), and C's spike start and length.
func realScenario(name string, duration, period, burst, spikeStart, spikeLen float64) Scenario {
	in, out := RealLengths()
	return Scenario{
		Name:     name,
		Duration: seconds(duration),
		Tenants: []TenantPattern{
			{Tenant: "A", Kind: KindSteady, BaseRPS: realRateA, PeakRPS: realRateA, CV: 1.2,
				Mix: interactiveMix, In: in, Out: out},
			{Tenant: "B", Kind: KindPeriodic, BaseRPS: realRateB, PeakRPS: realPeakB,
				PeriodSeconds: period, BurstSeconds: burst, PhaseSeconds: burst, CV: 1.5,
				Mix: periodicMix, In: in, Out: out},
			{Tenant: "C", Kind: KindSpike, BaseRPS: realRateC, PeakRPS: realPeakC,
				SpikeStartSeconds: spikeStart, SpikeDurationSeconds: spikeLen, CV: 2,
				Mix: batchMix, In: in, Out: out},
		},
	}
}
