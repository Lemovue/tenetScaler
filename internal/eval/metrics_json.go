package eval

import "encoding/json"

// runMetricsJSON is the JSON form of RunMetrics: the default field names,
// with null for values that are not finite.
type runMetricsJSON struct {
	VictimViolationPct, VictimShedAwareViolationPct *float64
	P99TTFT, P99TPOT                                map[string]*float64
	NoisyShedPct                                    *float64
	ReactionTimeSec                                 *float64
	ReplicaMinutes                                  *float64
	JainIndex                                       *float64
	MaxCounterGap                                   *float64
	ReclaimLatencySec                               *float64
	PolicyLatencyMeanMs                             *float64
	Completed, Shed, Failed                         int
}

// MarshalJSON encodes m under its field names, writing NaN and infinite
// values (scalars and p99 entries alike) as null: encoding/json rejects
// NaN, and undefined metrics are routine, for example the reclaim latency
// of a run without a reclaim phase or the p99 of a tenant without data.
func (m RunMetrics) MarshalJSON() ([]byte, error) {
	return json.Marshal(runMetricsJSON{
		VictimViolationPct:          jsonFloat(m.VictimViolationPct),
		VictimShedAwareViolationPct: jsonFloat(m.VictimShedAwareViolationPct),
		P99TTFT:                     jsonFloats(m.P99TTFT),
		P99TPOT:                     jsonFloats(m.P99TPOT),
		NoisyShedPct:                jsonFloat(m.NoisyShedPct),
		ReactionTimeSec:             jsonFloat(m.ReactionTimeSec),
		ReplicaMinutes:              jsonFloat(m.ReplicaMinutes),
		JainIndex:                   jsonFloat(m.JainIndex),
		MaxCounterGap:               jsonFloat(m.MaxCounterGap),
		ReclaimLatencySec:           jsonFloat(m.ReclaimLatencySec),
		PolicyLatencyMeanMs:         jsonFloat(m.PolicyLatencyMeanMs),
		Completed:                   m.Completed,
		Shed:                        m.Shed,
		Failed:                      m.Failed,
	})
}

// UnmarshalJSON decodes the form MarshalJSON writes; null or a missing
// scalar becomes NaN, and so does a null p99 entry.
func (m *RunMetrics) UnmarshalJSON(data []byte) error {
	var v runMetricsJSON
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*m = RunMetrics{
		VictimViolationPct:          floatOrNaN(v.VictimViolationPct),
		VictimShedAwareViolationPct: floatOrNaN(v.VictimShedAwareViolationPct),
		P99TTFT:                     floatsOrNaN(v.P99TTFT),
		P99TPOT:                     floatsOrNaN(v.P99TPOT),
		NoisyShedPct:                floatOrNaN(v.NoisyShedPct),
		ReactionTimeSec:             floatOrNaN(v.ReactionTimeSec),
		ReplicaMinutes:              floatOrNaN(v.ReplicaMinutes),
		JainIndex:                   floatOrNaN(v.JainIndex),
		MaxCounterGap:               floatOrNaN(v.MaxCounterGap),
		ReclaimLatencySec:           floatOrNaN(v.ReclaimLatencySec),
		PolicyLatencyMeanMs:         floatOrNaN(v.PolicyLatencyMeanMs),
		Completed:                   v.Completed,
		Shed:                        v.Shed,
		Failed:                      v.Failed,
	}
	return nil
}

// jsonFloats maps each value through jsonFloat; a nil map stays nil.
func jsonFloats(m map[string]float64) map[string]*float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]*float64, len(m))
	for k, v := range m {
		out[k] = jsonFloat(v)
	}
	return out
}

// floatsOrNaN maps each value through floatOrNaN; a nil map stays nil.
func floatsOrNaN(m map[string]*float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = floatOrNaN(v)
	}
	return out
}
