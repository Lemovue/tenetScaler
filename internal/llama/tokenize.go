package llama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TokenCounter returns a token counter for the gateway (gateway
// Config.TokenCounter) that asks a replica's own tokenizer: POST
// {endpoint}/tokenize {"content": prompt} answers {"tokens": [...]}, and the
// count is the number of tokens (decision Q17).
func TokenCounter() func(ctx context.Context, endpoint, prompt string) (int, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.IdleConnTimeout = 2 * time.Second // llama-server closes idle connections after about 5 s
	hc := &http.Client{Transport: tr}
	return func(ctx context.Context, endpoint, prompt string) (int, error) {
		body, err := json.Marshal(map[string]string{"content": prompt})
		if err != nil {
			return 0, err
		}
		url := strings.TrimRight(endpoint, "/") + "/tokenize"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		// Tokenizing has no effect, so the transport may resend it on a new
		// connection when a reused one was closed by the replica.
		req.Header["X-Idempotency-Key"] = nil
		resp, err := hc.Do(req)
		if err != nil {
			return 0, fmt.Errorf("tokenize: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			return 0, fmt.Errorf("tokenize: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
		var out struct {
			Tokens []json.RawMessage `json:"tokens"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
			return 0, fmt.Errorf("tokenize: decode: %w", err)
		}
		return len(out.Tokens), nil
	}
}
