package sim

import (
	"container/heap"
	"time"

	"tenantscaler/internal/policy"
)

// eventKind identifies what an event does.
type eventKind uint8

const (
	evArrival eventKind = iota // a request arrives at the gateway
	evReady                    // a starting replica finished loading
	evDone                     // a request produced its last token
	evTick                     // a control cycle
	evPolicy                   // a policy reaches the gateway
	evSample                   // a timeline sample
	evTimeout                  // a request's gateway dispatch timeout expires
)

// event is one scheduled occurrence. Events are ordered by time, then by
// the order they were scheduled in (seq), which makes runs deterministic.
type event struct {
	at   time.Duration
	seq  uint64
	kind eventKind
	req  *request
	rep  *replica
	pol  *policy.Policy
}

// eventHeap is a min-heap of events by (at, seq).
type eventHeap []*event

// Len, Less, Swap, Push and Pop implement heap.Interface.
func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(*event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old) - 1
	e := old[n]
	old[n] = nil
	*h = old[:n]
	return e
}

// schedule queues ev at virtual time at.
func (s *simulator) schedule(at time.Duration, ev *event) {
	ev.at, ev.seq = at, s.seq
	s.seq++
	heap.Push(&s.events, ev)
}
