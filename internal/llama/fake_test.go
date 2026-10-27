package llama

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary doubles as a fake llama-server: TestMain copies it to
// <tmp>/llama-server(.exe), and a copy started with envFakeServer=1 serves
// the llama-server endpoints the Process uses. Its image name therefore
// matches the real server's, which lets Cleanup tests run without llama.cpp.
const (
	envFakeServer = "TS_LLAMA_FAKE_SERVER" // "1": act as llama-server
	envFakeParent = "TS_LLAMA_FAKE_PARENT" // "1": launch one replica, print its PID, hang
	envFakeDir    = "TS_LLAMA_FAKE_DIR"    // parent mode: log and PID-file directory
	envFakeMode   = "TS_LLAMA_FAKE_MODE"   // "crash" | "never-ready" | "metrics-broken" | "" (normal)
	envFakeLoad   = "TS_LLAMA_FAKE_LOAD"   // simulated model-load time, e.g. "300ms"
	fakeTokenTime = 10 * time.Millisecond  // fake decode time per token
)

// fakeServerPath is the copied test binary acting as llama-server.
var fakeServerPath string

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(envFakeParent) == "1":
		os.Exit(runFakeParent())
	case os.Getenv(envFakeServer) == "1":
		os.Exit(runFakeServer(os.Args[1:]))
	}
	code, err := runTests(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "llama tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runTests(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "tenantscaler-llama-test-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	fakeServerPath = filepath.Join(dir, ServerImage)
	data, err := os.ReadFile(exe)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(fakeServerPath, data, 0o755); err != nil {
		return 0, err
	}
	// Children launched by the tests inherit this and act as fake servers;
	// the real llama-server ignores it.
	if err := os.Setenv(envFakeServer, "1"); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// runFakeParent stands in for a TenantScaler process that launches a
// replica and then dies without closing it (the orphan test).
func runFakeParent() int {
	_ = os.Unsetenv(envFakeParent) // the replica must act as a server
	dir := os.Getenv(envFakeDir)
	l := &Launcher{Spec: fakeSpec(), LogDir: dir, PIDFile: filepath.Join(dir, "llama.pids"), Logger: discardLogger}
	p, err := l.Launch("orphan-1", 0)
	if err != nil {
		fmt.Println("error:", err)
		return 1
	}
	if p.jobErr != nil {
		fmt.Println("error: orphan protection:", p.jobErr)
		return 1
	}
	fmt.Printf("pid %d\n", p.PID())
	for {
		time.Sleep(time.Hour) // until killed
	}
}

// runFakeServer imitates llama-server: it validates -m, prints its
// arguments (into the replica log), answers /health with 503 while
// "loading", and serves /metrics (500 in mode "metrics-broken") and
// streaming /v1/completions. GET /test/metrics-hits counts the /metrics
// requests, for tests of the Stats cache.
func runFakeServer(args []string) int {
	fmt.Println("fake llama-server args:", strings.Join(args, " "))
	opt := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			opt[args[i]] = args[i+1]
		}
	}
	mode := os.Getenv(envFakeMode)
	if mode == "crash" {
		fmt.Println("fake llama-server: simulated crash")
		return 3
	}
	if _, err := os.Stat(opt["-m"]); err != nil {
		fmt.Println("failed to load model:", err)
		return 1
	}
	load, _ := time.ParseDuration(os.Getenv(envFakeLoad))
	np, err := strconv.Atoi(opt["-np"])
	if err != nil || np < 1 {
		np = 1
	}
	e := &fakeEngine{
		slots: make(chan struct{}, np), readyAt: time.Now().Add(load),
		neverReady: mode == "never-ready", metricsBroken: mode == "metrics-broken",
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(opt["--host"], opt["--port"]))
	if err != nil {
		fmt.Println("bind:", err)
		return 1
	}
	fmt.Println(http.Serve(ln, e.handler()))
	return 1
}

// fakeEngine is the fake server's state.
type fakeEngine struct {
	slots                chan struct{} // -np concurrent requests
	readyAt              time.Time
	neverReady           bool
	metricsBroken        bool
	processing, deferred atomic.Int64
	seq                  atomic.Int64
	metricsHits          atomic.Int64
}

func (e *fakeEngine) ready() bool { return !e.neverReady && !time.Now().Before(e.readyAt) }

func (e *fakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if !e.ready() {
			http.Error(w, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		e.metricsHits.Add(1)
		if !e.ready() {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		if e.metricsBroken {
			http.Error(w, "fake llama-server: metrics broken", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "# HELP %[1]s Number of requests processing.\n# TYPE %[1]s gauge\n%[1]s %[2]d\n", metricProcessing, e.processing.Load())
		fmt.Fprintf(w, "# HELP %[1]s Number of requests deferred.\n# TYPE %[1]s gauge\n%[1]s %[2]d\n", metricDeferred, e.deferred.Load())
	})
	mux.HandleFunc("GET /test/metrics-hits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, e.metricsHits.Load())
	})
	mux.HandleFunc("POST /v1/completions", e.complete)
	return mux
}

// complete streams max_tokens chunks, then a final chunk with
// finish_reason "length" and usage, then [DONE], like llama-server.
func (e *fakeEngine) complete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string `json:"prompt"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MaxTokens < 1 || !req.Stream {
		http.Error(w, "fake llama-server: want a streaming request with max_tokens >= 1", http.StatusBadRequest)
		return
	}
	e.deferred.Add(1)
	select {
	case e.slots <- struct{}{}:
		e.deferred.Add(-1)
	case <-r.Context().Done():
		e.deferred.Add(-1)
		return
	}
	e.processing.Add(1)
	defer func() { e.processing.Add(-1); <-e.slots }()

	id := fmt.Sprintf("cmpl-%d", e.seq.Add(1))
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	chunk := func(text string, finish any, usage any) bool {
		c := map[string]any{
			"id": id, "object": "text_completion", "model": "fake",
			"choices": []map[string]any{{"text": text, "index": 0, "logprobs": nil, "finish_reason": finish}},
		}
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for i := range req.MaxTokens {
		time.Sleep(fakeTokenTime)
		if !chunk(fmt.Sprintf(" t%d", i), nil, nil) {
			return
		}
	}
	prompt := len(strings.Fields(req.Prompt))
	usage := map[string]int{"prompt_tokens": prompt, "completion_tokens": req.MaxTokens, "total_tokens": prompt + req.MaxTokens}
	if chunk("", "length", usage) {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}
}
