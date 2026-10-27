package loadgen

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
)

func TestWriteRecordsCSV(t *testing.T) {
	recs := map[string][]eval.Record{
		"y": {{ID: 2, Tenant: "C", Priority: model.PrioritySheddable, Arrival: time.Second, Shed: true, ShedReason: "over share", OutputTokens: 9}},
		"x": {{ID: 1, Tenant: "A", Priority: model.PriorityCritical, Arrival: time.Second,
			FirstToken: 1500 * time.Millisecond, Done: 2500 * time.Millisecond, InputTokens: 4, OutputTokens: 11}},
	}
	counts := map[string]Counts{"x": {Completed: 1}, "y": {Shed: 1, Simulated: 1}}
	var buf bytes.Buffer
	if err := WriteRecordsCSV(&buf, Result{Records: recs, Counts: counts}); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || strings.Join(rows[0], ",") != strings.Join(RecordsCSVHeader, ",") {
		t.Fatalf("got rows %v", rows)
	}
	want := []string{"x", "1", "A", "critical", "1.000000", "1.500000", "2.500000", "0.500000", "0.100000", "false", "", "false", "4", "11", "false"}
	if strings.Join(rows[1], ",") != strings.Join(want, ",") {
		t.Errorf("row 1 = %v, want %v", rows[1], want)
	}
	if rows[2][0] != "y" || rows[2][7] != "" || rows[2][9] != "true" || rows[2][10] != "over share" || rows[2][14] != "true" {
		t.Errorf("row 2 = %v, want the shed record of the simulated target y with empty ttft", rows[2])
	}
}
