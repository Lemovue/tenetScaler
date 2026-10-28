package demo

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// mirrorMaxBody bounds a mirrored request body, and mirrorCopyTimeout how
// long the baseline's copy of a request may take.
const (
	mirrorMaxBody     = 1 << 20
	mirrorCopyTimeout = 5 * time.Minute
)

// mirror serves the public address of a side-by-side live run. Every
// completion goes to the TenantScaler stack's gateway, whose answer the
// client gets, and at the same moment a copy goes to the baseline stack's
// gateway, whose answer is read to the end and dropped: both stacks serve
// the same live traffic, and each measures it. Both gateways check the
// tenant keys (the client's Authorization is passed on); only completions
// and /healthz are served, as on the stacks' own public listener.
type mirror struct {
	primary, copy string // gateway base URLs
	client        *http.Client
	ctx           context.Context // the run's: copies end with it
	log           *slog.Logger
}

func newMirror(ctx context.Context, primary, copy string, log *slog.Logger) *mirror {
	return &mirror{primary: primary, copy: copy, ctx: ctx, log: log,
		client: &http.Client{Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64}}}
}

// NewMirror returns the mirroring handler on its own (tenantscaler mirror):
// completions go to the primary gateway (TenantScaler), whose answer the
// client gets, and a copy to the copy gateway (the baseline); copies end
// with ctx.
func NewMirror(ctx context.Context, primary, copy string, log *slog.Logger) http.Handler {
	return newMirror(ctx, strings.TrimRight(primary, "/"), strings.TrimRight(copy, "/"), log)
}

func (m *mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		m.forward(w, r, nil)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/completions":
		body, err := io.ReadAll(io.LimitReader(r.Body, mirrorMaxBody+1))
		if err != nil {
			http.Error(w, "read request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > mirrorMaxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		go m.sendCopy(r.Header.Clone(), body)
		m.forward(w, r, body)
	default:
		http.NotFound(w, r)
	}
}

// forward sends r (with body) to the primary gateway and streams the answer
// back, flushing as it arrives so the client's time to first token holds.
func (m *mirror) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := m.request(r.Context(), m.primary, r.Method, r.URL.RequestURI(), r.Header, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		http.Error(w, "gateway unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// sendCopy sends the baseline its copy of a completion and reads the
// answer to the end, so the baseline gateway measures the whole request.
func (m *mirror) sendCopy(header http.Header, body []byte) {
	ctx, cancel := context.WithTimeout(m.ctx, mirrorCopyTimeout)
	defer cancel()
	req, err := m.request(ctx, m.copy, http.MethodPost, "/v1/completions", header, body)
	if err != nil {
		return
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Debug("baseline copy failed", "err", err)
		}
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

// request builds a request to base+uri with the client's end-to-end
// headers.
func (m *mirror) request(ctx context.Context, base, method, uri string, header http.Header, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+uri, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
			"Content-Length", "Accept-Encoding":
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept-Encoding", "identity")
	return req, nil
}
