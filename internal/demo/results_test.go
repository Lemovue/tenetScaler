package demo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResultsEndpoint checks /api/results without any summary, with the
// real summary and the experiment's default output (<dir>/summary.json),
// with the per-run layout of the repository (<dir>/sim/<name>/summary.json)
// and with an invalid file.
func TestResultsEndpoint(t *testing.T) {
	opt := testOptions(t)
	_, ts := newTestServer(t, opt)
	var missing Results
	getJSON(t, ts.URL+"/api/results", &missing)
	if missing.Real.Present || missing.Real.Summary != nil || !strings.HasSuffix(missing.Real.Path, "real/summary.json") {
		t.Fatalf("missing real results %+v", missing.Real)
	}
	searched := []string{"sim/summary.json", "sim/*/summary.json", "/summary.json"}
	if missing.Simulated == nil || len(missing.Simulated) != 0 || len(missing.SimulatedSearched) != len(searched) {
		t.Fatalf("missing simulated results: %+v searched %v, want an empty list and %d locations",
			missing.Simulated, missing.SimulatedSearched, len(searched))
	}
	for i, suffix := range searched {
		if !strings.HasSuffix(missing.SimulatedSearched[i], suffix) {
			t.Errorf("searched location %d %q, want ...%s", i, missing.SimulatedSearched[i], suffix)
		}
	}

	write := func(rel, content string) {
		path := filepath.Join(opt.ResultsDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("real/summary.json", `{"label":"REAL inference","simulated":false}`)
	write("summary.json", `{"simulated":true}`)
	var found Results
	getJSON(t, ts.URL+"/api/results", &found)
	if !found.Real.Present || found.Real.Error != "" || !strings.Contains(string(found.Real.Summary), "REAL inference") {
		t.Fatalf("real results %+v", found.Real)
	}
	if len(found.Simulated) != 1 {
		t.Fatalf("simulated results %+v, want the experiment's default output alone", found.Simulated)
	}
	if top := found.Simulated[0]; !top.Present || top.Name != "" || !strings.HasSuffix(top.Path, "/summary.json") ||
		strings.Contains(top.Path, "sim/") || string(top.Summary) != `{"simulated":true}` {
		t.Fatalf("simulated results (summary.json) %+v", top)
	}

	// The repository's layout: one directory per simulated run; a
	// directory without a summary and a stray file are ignored.
	for _, name := range []string{"reclaim", "default", "budget6"} {
		write("sim/"+name+"/summary.json", `{"simulated":true,"scenario":"`+name+`"}`)
	}
	write("sim/notes.txt", "not a run")
	if err := os.MkdirAll(filepath.Join(opt.ResultsDir, "sim", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	var nested Results
	getJSON(t, ts.URL+"/api/results", &nested)
	wantNames := []string{"default", "budget6", "reclaim", ""}
	if len(nested.Simulated) != len(wantNames) {
		t.Fatalf("nested simulated results %+v, want %v", nested.Simulated, wantNames)
	}
	for i, name := range wantNames {
		rf := nested.Simulated[i]
		wantPath := "sim/" + name + "/summary.json"
		if name == "" {
			wantPath = "/summary.json"
		}
		if rf.Name != name || !rf.Present || rf.Error != "" || !strings.HasSuffix(rf.Path, wantPath) ||
			(name != "" && !strings.Contains(string(rf.Summary), `"scenario":"`+name+`"`)) {
			t.Errorf("simulated result %d: %+v, want run %q at ...%s", i, rf, name, wantPath)
		}
	}

	write("sim/summary.json", `{not json`)
	var broken Results
	getJSON(t, ts.URL+"/api/results", &broken)
	if len(broken.Simulated) != len(wantNames)+1 {
		t.Fatalf("with sim/summary.json: %+v", broken.Simulated)
	}
	if first := broken.Simulated[0]; !strings.HasSuffix(first.Path, "sim/summary.json") || first.Name != "" ||
		!first.Present || first.Error == "" || first.Summary != nil {
		t.Fatalf("invalid sim/summary.json: %+v", first)
	}
}
