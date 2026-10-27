package gateway

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Discovery lists the base URLs of the replicas the gateway may dispatch
// to. Endpoints must be safe to call from any goroutine and cheap: the
// dispatcher calls it on every wake-up.
type Discovery interface{ Endpoints() []string }

// Notifier is implemented by a Discovery that can announce endpoint
// changes. The gateway subscribes to it so that its dispatcher reacts at
// once instead of on its next safety tick. The returned function cancels
// the subscription.
type Notifier interface {
	Subscribe(fn func()) (cancel func())
}

// StaticDiscovery is a fixed endpoint list, e.g. from -endpoints or
// $TS_UPSTREAM_ENDPOINTS. Entries are normalised like every discovered
// endpoint (see normalizeEndpoints).
type StaticDiscovery []string

// Endpoints returns the normalised, sorted, de-duplicated endpoints.
func (s StaticDiscovery) Endpoints() []string { return normalizeEndpoints(s) }

// normalizeEndpoints trims blanks, trailing slashes and a trailing "/v1"
// (OpenAI-style base URLs such as Ollama's http://host:11434/v1 end in it,
// and the gateway appends /v1/completions itself), adds "http://" to
// entries without a scheme, drops empty entries, and returns the result
// sorted and de-duplicated.
func normalizeEndpoints(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.TrimRight(strings.TrimSpace(e), "/")
		e = strings.TrimRight(strings.TrimSuffix(e, "/v1"), "/")
		if e == "" {
			continue
		}
		if !strings.Contains(e, "://") {
			e = "http://" + e
		}
		out = append(out, e)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// minFetchTimeout bounds one PollingDiscovery fetch from below, so a short
// polling interval does not abort a slow but healthy discovery source.
const minFetchTimeout = 5 * time.Second

// PollingDiscovery refreshes its endpoint list by calling a fetch function
// periodically, e.g. pool.Client.Endpoints for a local pool, or a
// Kubernetes EndpointSlice lookup. On a fetch error the last good list is
// kept. It implements Notifier.
type PollingDiscovery struct {
	// Logger receives fetch failures and recoveries; nil means no logging.
	// Set it before Start.
	Logger *slog.Logger

	fetch   func(context.Context) ([]string, error)
	every   time.Duration
	started atomic.Bool

	mu        sync.RWMutex
	endpoints []string
	lastErr   error
	subs      map[int]func()
	nextSub   int
}

// NewPollingDiscovery returns a discovery that calls fetch every `every`
// (minimum 10 ms) once Start is called. Endpoints is empty until the first
// successful fetch.
func NewPollingDiscovery(fetch func(context.Context) ([]string, error), every time.Duration) *PollingDiscovery {
	return &PollingDiscovery{fetch: fetch, every: max(every, 10*time.Millisecond), subs: map[int]func(){}}
}

// Start fetches immediately and then every interval, in a goroutine that
// exits when ctx is done. It returns at once; calling it again has no
// effect.
func (d *PollingDiscovery) Start(ctx context.Context) {
	if !d.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		t := time.NewTicker(d.every)
		defer t.Stop()
		for {
			_ = d.Refresh(ctx) // failures are logged and remembered by Refresh
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Refresh fetches the endpoint list once. On success it replaces the list
// and notifies subscribers when the list changed; on failure it keeps the
// previous list and returns the error.
func (d *PollingDiscovery) Refresh(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, max(d.every, minFetchTimeout))
	eps, err := d.fetch(fctx)
	cancel()
	d.mu.Lock()
	wasErr := d.lastErr != nil
	d.lastErr = err
	if err != nil {
		d.mu.Unlock()
		if !wasErr && ctx.Err() == nil && d.Logger != nil {
			d.Logger.Warn("endpoint discovery failed; keeping the last known endpoints", "err", err)
		}
		return err
	}
	eps = normalizeEndpoints(eps)
	changed := !slices.Equal(eps, d.endpoints)
	d.endpoints = eps
	subs := slices.Collect(maps.Values(d.subs))
	d.mu.Unlock()
	if wasErr && d.Logger != nil {
		d.Logger.Info("endpoint discovery recovered", "endpoints", len(eps))
	}
	if changed {
		for _, fn := range subs {
			fn()
		}
	}
	return nil
}

// Endpoints returns a copy of the last successfully fetched list.
func (d *PollingDiscovery) Endpoints() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Clone(d.endpoints)
}

// Err returns the error of the last fetch, or nil when it succeeded.
func (d *PollingDiscovery) Err() error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastErr
}

// Subscribe registers fn to be called after every fetch that changed the
// endpoint list. fn must not block. The returned function unsubscribes.
func (d *PollingDiscovery) Subscribe(fn func()) (cancel func()) {
	d.mu.Lock()
	id := d.nextSub
	d.nextSub++
	d.subs[id] = fn
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.subs, id)
		d.mu.Unlock()
	}
}
