package pool

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

// defaultHTTP is used when Client.HTTP is nil.
var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// Client talks to a pool's HTTP API (see Pool.Handler).
type Client struct {
	// BaseURL is the pool manager's address, e.g. "http://127.0.0.1:8100".
	BaseURL string
	// HTTP is the client to use; nil means a client with a 10 s timeout.
	HTTP *http.Client
}

// Scale sets the pool's desired replica count (PUT /pool/scale).
func (c Client) Scale(ctx context.Context, n int) error {
	return c.do(ctx, http.MethodPut, "/pool/scale", ScaleRequest{Replicas: &n}, nil)
}

// Status fetches the pool status (GET /pool).
func (c Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.do(ctx, http.MethodGet, "/pool", nil, &st)
	return st, err
}

// Endpoints fetches the ready replica base URLs (GET /endpoints).
func (c Client) Endpoints(ctx context.Context) ([]string, error) {
	var er EndpointsResponse
	if err := c.do(ctx, http.MethodGet, "/endpoints", nil, &er); err != nil {
		return nil, err
	}
	return er.Endpoints, nil
}

// do sends one request; a non-nil in is sent as JSON and a non-nil out is
// decoded from a 2xx JSON response.
func (c Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("pool client: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(data)
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("pool client: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("pool client: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pool client: %s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("pool client: decode %s %s: %w", method, url, err)
	}
	return nil
}
