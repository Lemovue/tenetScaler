package fairq

import (
	"container/heap"

	"tenantscaler/internal/model"
)

// Priority bands used by ModeRoundRobin, lowest first.
const (
	bandSheddable = iota
	bandStandard
	bandCritical
	numBands
)

// band maps a priority to its round-robin band.
func band(p model.Priority) int {
	switch {
	case p.Critical():
		return bandCritical
	case p.Sheddable():
		return bandSheddable
	}
	return bandStandard
}

// fairLess orders requests inside a tenant in fair mode: higher effective
// priority first, then earliest deadline (no deadline last), then lower ID.
func fairLess(a, b *Request) bool {
	if a.EffectivePriority != b.EffectivePriority {
		return a.EffectivePriority > b.EffectivePriority
	}
	if !a.Deadline.Equal(b.Deadline) {
		switch {
		case a.Deadline.IsZero():
			return false
		case b.Deadline.IsZero():
			return true
		}
		return a.Deadline.Before(b.Deadline)
	}
	return a.ID < b.ID
}

// fifoLess orders requests first come, first served: by arrival, then ID.
func fifoLess(a, b *Request) bool {
	if !a.Arrival.Equal(b.Arrival) {
		return a.Arrival.Before(b.Arrival)
	}
	return a.ID < b.ID
}

// queue is a binary heap of requests ordered by less. Each queued request
// records its heap index so it can be removed in O(log n) on Cancel.
type queue struct {
	items []*Request
	less  func(a, b *Request) bool
}

func newQueue(less func(a, b *Request) bool) *queue { return &queue{less: less} }

// Len, Less, Swap, Push and Pop implement heap.Interface.
func (q *queue) Len() int           { return len(q.items) }
func (q *queue) Less(i, j int) bool { return q.less(q.items[i], q.items[j]) }

func (q *queue) Swap(i, j int) {
	q.items[i], q.items[j] = q.items[j], q.items[i]
	q.items[i].slot = i
	q.items[j].slot = j
}

func (q *queue) Push(x any) {
	r := x.(*Request)
	r.slot = len(q.items)
	q.items = append(q.items, r)
}

func (q *queue) Pop() any {
	n := len(q.items) - 1
	r := q.items[n]
	q.items[n] = nil
	q.items = q.items[:n]
	r.slot = -1
	return r
}

// add inserts r.
func (q *queue) add(r *Request) {
	r.q = q
	heap.Push(q, r)
}

// take removes and returns the first request; the queue must be non-empty.
func (q *queue) take() *Request {
	r := heap.Pop(q).(*Request)
	r.q = nil
	return r
}

// remove deletes r, which must be in this queue.
func (q *queue) remove(r *Request) {
	heap.Remove(q, r.slot)
	r.q = nil
}
