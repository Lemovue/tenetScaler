package demo

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The dataset buttons: the config lists every dataset with whether its
// file is there; a run on a trace dataset takes its request sizes from the
// file, fitted to the scenario, and says so in the state; an unknown or
// missing dataset is refused before anything starts.
func TestABRunOnDataset(t *testing.T) {
	var b strings.Builder
	b.WriteString("Timestamp,Model,Request tokens,Response tokens,Total tokens,Log Type\n")
	for i := range 300 {
		fmt.Fprintf(&b, "%d,ChatGPT,%d,%d,%d,API log\n", i, 100+10*i, 20+i, 120+11*i)
	}
	dir := t.TempDir()
	opt := testOptions(t)
	opt.BurstGPTPath = filepath.Join(dir, "BurstGPT_1.csv")
	opt.AzurePath = filepath.Join(dir, "missing.csv")
	if err := os.WriteFile(opt.BurstGPTPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, ts := newTestServer(t, opt)

	var cfg ConfigReply
	getJSON(t, ts.URL+"/api/config", &cfg)
	avail := map[string]bool{}
	for _, d := range cfg.Datasets {
		avail[d.Key] = d.Available
	}
	if len(cfg.Datasets) != 3 || !avail[DatasetSynthetic] || !avail[DatasetBurstGPT] || avail[DatasetAzure] {
		t.Fatalf("datasets %+v: want synthetic and BurstGPT available, Azure not", cfg.Datasets)
	}
	for _, body := range []string{`{"dataset":"azure"}`, `{"dataset":"other"}`} {
		if status, _, got := do(t, http.MethodPost, ts.URL+"/api/ab/start", []byte(body), nil); status != http.StatusBadRequest {
			t.Fatalf("start %s: %d %s, want 400", body, status, got)
		}
	}
	if st := srv.State(); st.Run != 0 {
		t.Fatalf("a refused dataset started run %d", st.Run)
	}

	if status, _, got := do(t, http.MethodPost, ts.URL+"/api/ab/start", []byte(`{"dataset":"burstgpt"}`), nil); status != http.StatusAccepted {
		t.Fatalf("start on BurstGPT: %d %s", status, got)
	}
	done := waitState(t, ts.URL, 60*time.Second, func(st State) bool { return st.Phase != PhaseRunning })
	if done.Phase != PhaseDone {
		t.Fatalf("run on BurstGPT ended %q: %s", done.Phase, done.Error)
	}
	d := done.Dataset
	if d == nil || d.Key != DatasetBurstGPT || d.Trace == nil || d.Trace.Rows != 300 || d.Sampled != 300 || d.Fit == nil {
		t.Fatalf("dataset of the run %+v: want BurstGPT, 300 rows sampled, fitted", d)
	}
	if d.Fit.InFactor >= 1 || !strings.Contains(d.Summary, "BurstGPT") || !strings.Contains(d.Summary, "stay synthetic") {
		t.Errorf("fit %+v, summary %q", d.Fit, d.Summary)
	}

	// The default stays the scenario's own sizes.
	if _, err := srv.StartAB(opt.Seed); err != nil {
		t.Fatal(err)
	}
	done = waitState(t, ts.URL, 60*time.Second, func(st State) bool { return st.Run == 2 && st.Phase != PhaseRunning })
	if done.Dataset == nil || done.Dataset.Key != DatasetSynthetic || done.Dataset.Fit != nil {
		t.Errorf("default run's dataset %+v, want synthetic", done.Dataset)
	}
}
