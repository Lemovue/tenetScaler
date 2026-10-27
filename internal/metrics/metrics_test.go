package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHandlerServesRegisteredMetrics(t *testing.T) {
	reg := NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tenantscaler_test_total", Help: "test"}, []string{"tenant"})
	reg.MustRegister(c)
	c.WithLabelValues("A").Add(3)

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{`tenantscaler_test_total{tenant="A"} 3`, "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestRegistriesAreIndependent(t *testing.T) {
	a, b := NewRegistry(), NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "tenantscaler_dup", Help: "dup"})
	a.MustRegister(g)
	if err := b.Register(prometheus.NewGauge(prometheus.GaugeOpts{Name: "tenantscaler_dup", Help: "dup"})); err != nil {
		t.Fatalf("second private registry should accept the same metric name: %v", err)
	}
}
