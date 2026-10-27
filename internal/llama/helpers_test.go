package llama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/config"
)

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSpec is a small valid spec; Server and Model come from the
// environment unless a test sets them.
func fakeSpec() config.LlamaSpec {
	return config.LlamaSpec{Threads: 1, Parallel: 2, CtxPerSlot: 128, ReadyTimeoutSeconds: 30}
}

// fakeLauncher launches the fake llama-server with a model file, logs and
// PID file under a fresh temporary directory.
func fakeLauncher(t *testing.T) *Launcher {
	t.Helper()
	dir := t.TempDir()
	model := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(model, []byte("fake gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := fakeSpec()
	spec.Server, spec.Model = fakeServerPath, model
	return &Launcher{Spec: spec, LogDir: filepath.Join(dir, "logs"), PIDFile: filepath.Join(dir, "llama.pids"), Logger: discardLogger}
}

// launch starts a replica and closes it when the test ends.
func launch(t *testing.T, l *Launcher, id string, slot int) *Process {
	t.Helper()
	p, err := l.Launch(id, slot)
	if err != nil {
		t.Fatalf("Launch(%s, %d): %v", id, slot, err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close %s: %v", id, err)
		}
	})
	return p
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readPIDs returns the distinct PIDs of a PID file, in file order.
func readPIDs(path string) ([]int, error) {
	entries, err := readPIDEntries(path)
	if err != nil {
		return nil, err
	}
	pids := make([]int, len(entries))
	for i, e := range entries {
		pids[i] = e.pid
	}
	return pids, nil
}

// metricsHits asks the fake llama-server how many /metrics requests it
// has served.
func metricsHits(t *testing.T, p *Process) int {
	t.Helper()
	resp, err := http.Get(p.URL() + "/test/metrics-hits")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("metrics hits %q: %v", body, err)
	}
	return n
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// assertGone fails unless none of pids is a running llama-server.
func assertGone(t *testing.T, pids ...int) {
	t.Helper()
	alive, err := llamaServerPIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		if alive[pid] {
			t.Fatalf("llama-server pid %d is still running", pid)
		}
	}
}

// completion is what a streamed /v1/completions response delivered.
type completion struct {
	TextChunks       int
	Text             string
	FinishReason     string
	CompletionTokens int
	Done             bool
	TTFT, Total      time.Duration
}

// streamCompletion sends one streaming request the way the gateway and
// loadgen do (ignore_eos, so exactly maxTokens tokens) and parses the SSE.
func streamCompletion(ctx context.Context, base, prompt string, maxTokens int) (completion, error) {
	var c completion
	body, _ := json.Marshal(map[string]any{
		"prompt": prompt, "max_tokens": maxTokens, "stream": true, "ignore_eos": true, "cache_prompt": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/completions", strings.NewReader(string(body)))
	if err != nil {
		return c, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return c, fmt.Errorf("status %s: %s", resp.Status, msg)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			c.Done = true
			break
		}
		var chunk struct {
			Choices []struct {
				Text         string  `json:"text"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return c, fmt.Errorf("bad chunk %q: %w", data, err)
		}
		if len(chunk.Choices) > 0 {
			ch := chunk.Choices[0]
			if ch.Text != "" {
				if c.TextChunks == 0 {
					c.TTFT = time.Since(start)
				}
				c.TextChunks++
				c.Text += ch.Text
			}
			if ch.FinishReason != nil {
				c.FinishReason = *ch.FinishReason
			}
		}
		if chunk.Usage != nil {
			c.CompletionTokens = chunk.Usage.CompletionTokens
		}
	}
	c.Total = time.Since(start)
	return c, sc.Err()
}
