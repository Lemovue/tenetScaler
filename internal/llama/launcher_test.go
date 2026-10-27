package llama

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/replica"
)

func TestArgs(t *testing.T) {
	l := &Launcher{Spec: config.LlamaSpec{Threads: 2, Parallel: 4, CtxPerSlot: 1024, ExtraArgs: []string{"--no-webui"}}}
	got := strings.Join(l.args("/m.gguf", "c", 4321), " ")
	want := "-m /m.gguf -ngl 0 -t 2 -C c --cpu-strict 1 -c 4096 -np 4 --metrics --host 127.0.0.1 --port 4321 --cache-ram 0 -dev none --no-webui"
	if got != want {
		t.Fatalf("args\n got %s\nwant %s", got, want)
	}
	l.Spec.CacheRAMMiB = 256
	if got := strings.Join(l.args("/m.gguf", "c", 1), " "); !strings.Contains(got, "--cache-ram 256") {
		t.Fatalf("cache-ram cap not passed: %s", got)
	}
	l.Spec.CacheRAMMiB = 0
	l.Spec.GPULayers = 99
	if got := strings.Join(l.args("/m.gguf", "3", 1), " "); strings.Contains(got, "-dev none") || !strings.Contains(got, "-ngl 99") {
		t.Fatalf("with GPU layers: %s", got)
	}
}

func TestDefaultCPUMask(t *testing.T) {
	tests := map[int]string{0: "3", 1: "c", 2: "30", 3: "c0", 7: "c000", 40: "3" + strings.Repeat("0", 20)}
	for slot, want := range tests {
		if got := DefaultCPUMask(slot); got != want {
			t.Errorf("DefaultCPUMask(%d) = %q, want %q", slot, got, want)
		}
	}
}

func TestCPUMaskSelection(t *testing.T) {
	l := &Launcher{Spec: config.LlamaSpec{CPUMasks: []string{"0x1", "2"}}}
	if m, err := l.cpuMask(1); err != nil || m != "2" {
		t.Fatalf("configured mask: %q, %v", m, err)
	}
	if _, err := l.cpuMask(2); err == nil || !strings.Contains(err.Error(), "no entry in backend.llama.cpuMasks") {
		t.Fatalf("slot beyond masks: %v", err)
	}
	l.Spec.CPUMasks = nil
	if m, err := l.cpuMask(1); err != nil || m != "c" {
		t.Fatalf("default mask: %q, %v", m, err)
	}
	if _, err := l.cpuMask(runtime.NumCPU()); err == nil || !strings.Contains(err.Error(), "P-core") && !strings.Contains(err.Error(), "logical CPUs") {
		t.Fatalf("slot beyond the machine's cores: %v", err)
	}
	if _, err := l.cpuMask(-1); err == nil {
		t.Fatal("negative slot accepted")
	}
}

// TestLaunchErrors checks the clear errors for missing paths and bad input.
// The environment fallbacks are cleared so a developer's real paths cannot
// leak in.
func TestLaunchErrors(t *testing.T) {
	t.Setenv(EnvServer, "")
	t.Setenv(EnvModel, "")
	dir := t.TempDir()
	model := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(model, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	base := fakeSpec()
	tests := []struct {
		name string
		id   string
		edit func(*config.LlamaSpec)
		want string
	}{
		{"no server", "r-1", func(s *config.LlamaSpec) { s.Model = model }, "set backend.llama.server or $TS_LLAMA_SERVER"},
		{"server missing", "r-1", func(s *config.LlamaSpec) { s.Server, s.Model = filepath.Join(dir, "nope.exe"), model }, "does not exist"},
		{"server is a dir", "r-1", func(s *config.LlamaSpec) { s.Server, s.Model = dir, model }, "is a directory"},
		{"no model", "r-1", func(s *config.LlamaSpec) { s.Server = fakeServerPath }, "set backend.llama.model or $TS_LLAMA_MODEL"},
		{"model missing", "r-1", func(s *config.LlamaSpec) { s.Server, s.Model = fakeServerPath, model+".x" }, "does not exist"},
		{"invalid spec", "r-1", func(s *config.LlamaSpec) { s.Threads = 0 }, "threads must be >= 1"},
		{"id with separator", "a/b", func(s *config.LlamaSpec) { s.Server, s.Model = fakeServerPath, model }, "plain file name"},
		{"empty id", "", func(s *config.LlamaSpec) { s.Server, s.Model = fakeServerPath, model }, "plain file name"},
	}
	for _, tc := range tests {
		spec := base
		tc.edit(&spec)
		l := &Launcher{Spec: spec, LogDir: dir, PIDFile: filepath.Join(dir, "pids"), Logger: discardLogger}
		if _, err := l.Launch(tc.id, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to contain %q", tc.name, err, tc.want)
		}
		if err := l.Check(); tc.id != "r-1" && err != nil {
			t.Errorf("%s: Check() = %v, want nil (only the id is wrong)", tc.name, err)
		} else if tc.id == "r-1" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: Check() = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	ok := fakeSpec()
	ok.Server, ok.Model = fakeServerPath, model
	if err := (&Launcher{Spec: ok}).Check(); err != nil {
		t.Fatalf("Check of a valid spec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pids")); !os.IsNotExist(err) {
		t.Fatalf("failed launches wrote a pid file: %v", err)
	}
}

func TestParseMetrics(t *testing.T) {
	real := `# HELP llamacpp:prompt_tokens_total Number of prompt tokens processed.
# TYPE llamacpp:prompt_tokens_total counter
llamacpp:prompt_tokens_total 1200
# HELP llamacpp:requests_processing Number of requests processing.
# TYPE llamacpp:requests_processing gauge
llamacpp:requests_processing 3
# HELP llamacpp:requests_deferred Number of requests deferred.
# TYPE llamacpp:requests_deferred gauge
llamacpp:requests_deferred 2
`
	tests := []struct {
		name, in string
		want     replica.Stats
		err      string
	}{
		{"llama-server output", real, replica.Stats{Running: 3, Waiting: 2}, ""},
		{"labels, float and timestamp", "llamacpp:requests_processing{model=\"q\"} 1.0 1700000000\nllamacpp:requests_deferred 0e0\n", replica.Stats{Running: 1}, ""},
		{"missing deferred", "llamacpp:requests_processing 1\n", replica.Stats{}, "requests_deferred not found"},
		{"bad value", "llamacpp:requests_processing x\nllamacpp:requests_deferred 0\n", replica.Stats{}, "bad value"},
		{"negative value", "llamacpp:requests_processing -1\nllamacpp:requests_deferred 0\n", replica.Stats{}, "bad value"},
		{"empty body", "", replica.Stats{}, "not found"},
	}
	for _, tc := range tests {
		got, err := parseMetrics(strings.NewReader(tc.in))
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestReadPIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pids")
	data := "12 r-1 2026-10-26T10:00:00Z\n\nnot-a-pid\n-4 bad\n0 zero\n34 r-2\n12 r-1-again\n" +
		"56 r-5 not-a-time\n34 r-2-reused 2026-10-26T11:00:00.5+05:30\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	pids, err := readPIDs(path)
	if err != nil || len(pids) != 3 || pids[0] != 12 || pids[1] != 34 || pids[2] != 56 {
		t.Fatalf("readPIDs = %v, %v; want [12 34 56]", pids, err)
	}
	entries, err := readPIDEntries(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{
		time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),                         // a later line without a time keeps it
		time.Date(2026, 10, 26, 5, 30, 0, 500*int(time.Millisecond), time.UTC), // the last timed line wins
		{}, // no valid time: checked by image name only
	}
	for i, e := range entries {
		if !e.launched.Equal(want[i]) {
			t.Errorf("pid %d launched %v, want %v", e.pid, e.launched, want[i])
		}
	}
}

// TestLogAppends checks that relaunching a replica ID keeps the previous
// launch's log: IDs restart at "<pool>-1" in every run.
func TestLogAppends(t *testing.T) {
	t.Setenv(envFakeMode, "crash")
	l := fakeLauncher(t)
	var logPath string
	for range 2 {
		p := launch(t, l, "again-1", 0)
		waitFor(t, 30*time.Second, "crashed", func() bool { return p.State() == replica.StateStopped })
		logPath = p.LogPath()
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"launching replica again-1", "simulated crash"} {
		if n := strings.Count(string(data), want); n != 2 {
			t.Fatalf("log has %q %d times, want 2 (one per launch):\n%s", want, n, data)
		}
	}
}
