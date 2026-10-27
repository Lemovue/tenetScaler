package loadgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// discard is a logger that drops everything.
var discard = slog.New(slog.DiscardHandler)

// received is one request seen by a fake server.
type received struct {
	Tenant, Priority string
	Body             completionRequest
	// Raw is the body decoded as a generic JSON object, to check which
	// fields were actually sent.
	Raw map[string]any
}

// key identifies the traffic content of a request (not its timing).
func (r received) key() string {
	return fmt.Sprintf("%s|%s|%d|%s", r.Tenant, r.Priority, r.Body.MaxTokens, r.Body.Prompt)
}

// fakeLLM is an OpenAI-compatible streaming completion server shaped like
// llama-server: one chunk per token (or tokensPerChunk), a final chunk with
// finish_reason "length" and usage, then "data: [DONE]".
type fakeLLM struct {
	firstDelay     time.Duration // before the first chunk ("prefill")
	tokenDelay     time.Duration // between chunks
	tokensPerChunk int           // default 1
	noUsage        bool          // omit the usage object
	extraPrompt    int           // added to the prompt word count in usage.prompt_tokens
	noDone         bool          // end after the final chunk without [DONE]
	cutAfterFirst  bool          // end the stream right after the first chunk
	block          chan struct{} // when set, wait for it to close before answering

	mu  sync.Mutex
	got []received
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/completions" {
		http.NotFound(w, r)
		return
	}
	data, err := io.ReadAll(r.Body)
	var body completionRequest
	var raw map[string]any
	if err == nil {
		err = errors.Join(json.Unmarshal(data, &body), json.Unmarshal(data, &raw))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.got = append(f.got, received{Tenant: r.Header.Get(TenantHeader), Priority: r.Header.Get(PriorityHeader), Body: body, Raw: raw})
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	time.Sleep(f.firstDelay)
	per := max(f.tokensPerChunk, 1)
	for sent := 0; sent < body.MaxTokens; sent += per {
		if sent > 0 {
			time.Sleep(f.tokenDelay)
		}
		n := min(per, body.MaxTokens-sent)
		fmt.Fprintf(w, "data: {\"choices\":[{\"text\":%q,\"index\":0,\"finish_reason\":null}]}\n\n", strings.Repeat("tok ", n))
		_ = rc.Flush()
		if f.cutAfterFirst {
			return
		}
	}
	usage := ""
	if !f.noUsage {
		prompt := len(strings.Fields(body.Prompt)) + f.extraPrompt
		usage = fmt.Sprintf(`,"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`,
			prompt, body.MaxTokens, prompt+body.MaxTokens)
	}
	fmt.Fprintf(w, "data: {\"choices\":[{\"text\":\"\",\"index\":0,\"finish_reason\":\"length\"}]%s}\n\n", usage)
	if !f.noDone {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	_ = rc.Flush()
}

// requests returns a copy of the requests seen so far.
func (f *fakeLLM) requests() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.got...)
}

// countTenant counts the requests of one tenant seen so far.
func (f *fakeLLM) countTenant(tenant string) int {
	n := 0
	for _, r := range f.requests() {
		if r.Tenant == tenant {
			n++
		}
	}
	return n
}

// serve starts h on 127.0.0.1:0 and closes it when the test ends.
func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
