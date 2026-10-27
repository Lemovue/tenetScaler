// Package e2e holds the end-to-end smoke tests: they build and start the
// real tenantscaler binary, check /healthz and the dashboard's files, drive
// the dashboard's own HTTP API through a full A/B run on the 6 s "tiny"
// scenario, and stop the process by PID. Run them with
// `go test -p 1 -v ./test/e2e/...` (scripts/test.ps1 does).
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// repoRoot is the repository, two levels up from this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// buildBinary builds cmd/tenantscaler into a temporary directory.
func buildBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tenantscaler")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "./cmd/tenantscaler")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// demo is a running `tenantscaler demo` process.
type demo struct {
	base string
	cmd  *exec.Cmd
	log  *bytes.Buffer
}

// startDemo starts the demo with the given backend on a free port and
// waits for /healthz; the process is stopped by PID when the test ends.
func startDemo(t *testing.T, exe, backend string) *demo {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	var log bytes.Buffer
	cmd := exec.Command(exe, "demo", "-backend", backend, "-scenario", "tiny", "-addr", addr, "-log-dir", t.TempDir())
	cmd.Dir = repoRoot(t)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &demo{base: "http://" + addr, cmd: cmd, log: &log}
	t.Cleanup(func() { d.stop(t) })
	deadline := time.Now().Add(60 * time.Second)
	for {
		if resp, err := http.Get(d.base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no /healthz within 60 s; log:\n%s", log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stop kills the process by PID, waits for it, and checks the port closed.
func (d *demo) stop(t *testing.T) {
	if d.cmd.ProcessState != nil {
		return
	}
	pid := d.cmd.Process.Pid
	_ = d.cmd.Process.Kill()
	_ = d.cmd.Wait()
	if _, err := http.Get(d.base + "/healthz"); err == nil {
		t.Errorf("pid %d stopped but %s still answers", pid, d.base)
	}
}

func (d *demo) get(t *testing.T, path string, into any) []byte {
	t.Helper()
	resp, err := http.Get(d.base + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	if into != nil {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("GET %s: %v in %s", path, err, body)
		}
	}
	return body
}

type state struct {
	Phase  string `json:"phase"`
	Error  string `json:"error"`
	Stacks []struct {
		Key     string          `json:"key"`
		Status  string          `json:"status"`
		Metrics json.RawMessage `json:"metrics"`
	} `json:"stacks"`
}

// runAB starts an A/B run through the API and waits until it is done.
func runAB(t *testing.T, d *demo, limit time.Duration) state {
	t.Helper()
	resp, err := http.Post(d.base+"/api/ab/start", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/ab/start: %d %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(limit)
	for {
		var s state
		d.get(t, "/api/state", &s)
		switch s.Phase {
		case "done":
			return s
		case "error", "stopped":
			t.Fatalf("run ended in %s: %s; log:\n%s", s.Phase, s.Error, d.log.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("run not done within %s (phase %s); log:\n%s", limit, s.Phase, d.log.String())
		}
		time.Sleep(time.Second)
	}
}

func checkDone(t *testing.T, s state) {
	t.Helper()
	if len(s.Stacks) != 2 {
		t.Fatalf("want 2 stacks, got %d", len(s.Stacks))
	}
	for _, st := range s.Stacks {
		if st.Status != "done" || len(st.Metrics) == 0 || string(st.Metrics) == "null" {
			t.Errorf("stack %s: status %s, metrics %s", st.Key, st.Status, st.Metrics)
		}
	}
}

// The demo on the SIMULATED engine: dashboard files, config, one full A/B.
func TestDemoSmoke(t *testing.T) {
	exe := buildBinary(t)
	d := startDemo(t, exe, "simulated")

	page := string(d.get(t, "/", nil))
	for _, want := range []string{"<title>", `<script src="app.js" defer></script>`, `id="theme-toggle"`} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard page lacks %q", want)
		}
	}
	for _, f := range []string{"/style.css", "/app.js", "/chart.js", "/decisions.js", "/theme.js", "/decor.js", "/comic.js",
		"/spider3d.js", "/vendor/three.module.min.js", "/fonts/Bangers-Regular.ttf"} {
		d.get(t, f, nil)
	}

	var cfg struct {
		Backend struct {
			Simulated bool `json:"simulated"`
		} `json:"backend"`
		Scenario struct {
			Name string `json:"name"`
		} `json:"scenario"`
	}
	d.get(t, "/api/config", &cfg)
	if !cfg.Backend.Simulated || cfg.Scenario.Name != "tiny" {
		t.Fatalf("config: simulated %v, scenario %q", cfg.Backend.Simulated, cfg.Scenario.Name)
	}

	checkDone(t, runAB(t, d, 3*time.Minute))
	var dec struct {
		Records []json.RawMessage `json:"records"`
	}
	d.get(t, "/api/decisions?limit=5", &dec) // the decision log answers after a run
	d.get(t, "/api/results", nil)
}

// The same on real llama.cpp replicas; skipped unless TS_LLAMA_SERVER and
// TS_LLAMA_MODEL are set in the environment.
func TestRealReplicaSmoke(t *testing.T) {
	if os.Getenv("TS_LLAMA_SERVER") == "" || os.Getenv("TS_LLAMA_MODEL") == "" {
		t.Skip("TS_LLAMA_SERVER and TS_LLAMA_MODEL are not set")
	}
	exe := buildBinary(t)
	d := startDemo(t, exe, "llama")
	checkDone(t, runAB(t, d, 6*time.Minute))
}
