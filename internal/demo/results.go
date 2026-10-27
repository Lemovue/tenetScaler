package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// maxSummaryBytes caps the size of a summary file /api/results serves.
const maxSummaryBytes = 16 << 20

// defaultSimRun is the simulated run listed first: the default scenario
// at the default budget (`tenantscaler experiment -out results/sim/default`).
const defaultSimRun = "default"

// ResultFile is one evaluation summary as GET /api/results serves it.
type ResultFile struct {
	// Path is the file (or, for an absent real summary, where it was
	// looked for).
	Path string `json:"path"`
	// Name is a simulated run's name: the subdirectory of <dir>/sim that
	// holds it (for example default, budget6, reclaim); empty for a
	// summary directly in <dir>/sim or <dir>, and for the real summary.
	Name string `json:"name,omitempty"`
	// Present is true when the file exists; Error is set when it exists
	// but cannot be read or is not JSON.
	Present bool   `json:"present"`
	Error   string `json:"error,omitempty"`
	// Summary is the file's JSON as written by `tenantscaler realrun`
	// (real) or `tenantscaler experiment` (simulated); its own label and
	// simulated flag say how its numbers were produced.
	Summary json.RawMessage `json:"summary,omitempty"`
}

// Results are the evaluation summaries GET /api/results serves.
type Results struct {
	// Real is <dir>/real/summary.json, from REAL replicas.
	Real ResultFile `json:"real"`
	// Simulated are the summaries of the virtual-time simulation found, in
	// display order: <dir>/sim/summary.json, every
	// <dir>/sim/<name>/summary.json (default first, then by name) and
	// <dir>/summary.json (the experiment command's default output). Empty
	// when none exists.
	Simulated []ResultFile `json:"simulated"`
	// SimulatedSearched are the locations searched for simulated
	// summaries, for the empty state.
	SimulatedSearched []string `json:"simulatedSearched"`
}

// LoadResults reads the evaluation summaries under dir. Missing files are
// reported as not present (Real) or left out (Simulated), never as an
// error.
func LoadResults(dir string) Results {
	simDir := filepath.Join(dir, "sim")
	realSummary, _ := loadSummary(filepath.Join(dir, "real", "summary.json"))
	res := Results{
		Real:      realSummary,
		Simulated: []ResultFile{},
		SimulatedSearched: []string{
			filepath.ToSlash(filepath.Join(simDir, "summary.json")),
			filepath.ToSlash(filepath.Join(simDir, "*", "summary.json")),
			filepath.ToSlash(filepath.Join(dir, "summary.json")),
		},
	}
	add := func(path, name string) {
		if rf, exists := loadSummary(path); exists {
			rf.Name = name
			res.Simulated = append(res.Simulated, rf)
		}
	}
	add(filepath.Join(simDir, "summary.json"), "")
	for _, name := range simRuns(simDir) {
		add(filepath.Join(simDir, name, "summary.json"), name)
	}
	add(filepath.Join(dir, "summary.json"), "")
	return res
}

// simRuns lists the subdirectories of simDir, defaultSimRun first and the
// others by name; none when simDir cannot be read.
func simRuns(simDir string) []string {
	entries, err := os.ReadDir(simDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		if (names[i] == defaultSimRun) != (names[j] == defaultSimRun) {
			return names[i] == defaultSimRun
		}
		return names[i] < names[j]
	})
	return names
}

// loadSummary reads one summary file; exists is false when it is absent
// (rf then holds only its path).
func loadSummary(path string) (rf ResultFile, exists bool) {
	rf.Path = filepath.ToSlash(path)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return rf, false
	}
	rf.Present = true
	if err != nil {
		rf.Error = err.Error()
		return rf, true
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSummaryBytes+1))
	switch {
	case err != nil:
		rf.Error = fmt.Sprintf("read %s: %v", rf.Path, err)
	case len(data) > maxSummaryBytes:
		rf.Error = fmt.Sprintf("%s is larger than %d MiB", rf.Path, maxSummaryBytes>>20)
	case !json.Valid(data):
		rf.Error = fmt.Sprintf("%s is not valid JSON", rf.Path)
	default:
		rf.Summary = data
	}
	return rf, true
}
