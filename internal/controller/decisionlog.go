package controller

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"tenantscaler/internal/model"
)

// DefaultLogCapacity is the ring size NewDecisionLog uses for capacity <= 0.
const DefaultLogCapacity = 2000

// DecisionLog keeps the most recent decision records in a ring buffer and,
// optionally, appends every record to a JSON Lines file. It is safe for
// concurrent use.
type DecisionLog struct {
	mu    sync.Mutex
	ring  []model.DecisionRecord
	start int // index of the oldest record
	n     int // records held
	file  *os.File
	w     *bufio.Writer
	enc   *json.Encoder
	err   error // file error of the latest Add
}

// NewDecisionLog returns a log holding up to capacity records (capacity <= 0
// means DefaultLogCapacity). A non-empty jsonlPath is opened for appending,
// creating it and its directory if needed.
func NewDecisionLog(capacity int, jsonlPath string) (*DecisionLog, error) {
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	l := &DecisionLog{ring: make([]model.DecisionRecord, capacity)}
	if jsonlPath == "" {
		return l, nil
	}
	if dir := filepath.Dir(jsonlPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("decision log: create directory: %w", err)
		}
	}
	f, err := os.OpenFile(jsonlPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	l.file = f
	l.w = bufio.NewWriter(f)
	l.enc = json.NewEncoder(l.w)
	return l, nil
}

// Add appends records, oldest first. When the ring is full the oldest
// records are dropped. File write errors do not lose the in-memory records;
// Err reports them.
func (l *DecisionLog) Add(recs ...model.DecisionRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range recs {
		capacity := len(l.ring)
		if l.n < capacity {
			l.ring[(l.start+l.n)%capacity] = r
			l.n++
		} else {
			l.ring[l.start] = r
			l.start = (l.start + 1) % capacity
		}
	}
	l.err = nil
	if l.enc == nil || len(recs) == 0 {
		return
	}
	for _, r := range recs {
		if err := l.enc.Encode(r); err != nil {
			l.err = fmt.Errorf("decision log: write %s: %w", l.file.Name(), err)
			return
		}
	}
	if err := l.w.Flush(); err != nil {
		l.err = fmt.Errorf("decision log: write %s: %w", l.file.Name(), err)
	}
}

// Err returns the file error of the most recent Add, or nil.
func (l *DecisionLog) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// List returns up to limit records, newest first (limit <= 0 means all),
// only those of tenant when tenant is not empty.
func (l *DecisionLog) List(limit int, tenant string) []model.DecisionRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []model.DecisionRecord{}
	for i := l.n - 1; i >= 0; i-- {
		if limit > 0 && len(out) >= limit {
			break
		}
		r := l.ring[(l.start+i)%len(l.ring)]
		if tenant != "" && r.Tenant != tenant {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Len returns the number of records held in memory.
func (l *DecisionLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// Close flushes and closes the JSONL file, if any. Records can still be
// added afterwards; they are then kept in memory only.
func (l *DecisionLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := errors.Join(l.w.Flush(), l.file.Close())
	l.file, l.w, l.enc = nil, nil, nil
	if err != nil {
		return fmt.Errorf("decision log: close: %w", err)
	}
	return nil
}
