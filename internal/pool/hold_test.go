package pool

import (
	"testing"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/replica"
)

// A held replica is reported as starting, and kept out of the endpoints,
// until the hold has passed although its engine is ready earlier; then its
// cold start is the hold.
func TestReadinessHold(t *testing.T) {
	params := engine.DefaultParams()
	params.StartupDuration = 20 * time.Millisecond
	const hold = 400 * time.Millisecond
	p := New(Config{Name: "held", MaxReplicas: 1, Backend: WithReadinessHold(NewSimulatedBackend(params, 1), hold)})
	defer p.Close()
	launched := time.Now()
	if err := p.Scale(1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // the engine is ready, the hold is not over
	if st := p.Status(); st.Ready != 0 || st.Starting != 1 || len(p.Endpoints()) != 0 {
		t.Fatalf("inside the hold: %+v, endpoints %v; want starting and not discoverable", st, p.Endpoints())
	}
	for p.Status().Ready == 0 {
		if time.Since(launched) > 5*time.Second {
			t.Fatal("never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waited := time.Since(launched); waited < hold {
		t.Fatalf("ready after %v, before the %v hold", waited, hold)
	}
	st := p.Status()
	if len(p.Endpoints()) != 1 || st.Replicas[0].State != replica.StateReady || st.Replicas[0].ColdStartSeconds != hold.Seconds() {
		t.Fatalf("after the hold: %+v; want ready, discoverable, cold start = hold", st)
	}
	if WithReadinessHold(NewSimulatedBackend(params, 1), 0) == nil {
		t.Fatal("no hold must return the backend")
	}
}
