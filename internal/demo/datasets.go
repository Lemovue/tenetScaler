package demo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tenantscaler/internal/workload"
)

// Datasets an A/B run can take its request sizes from (the dashboard's
// dataset buttons; POST /api/ab/start {"dataset": key}).
const (
	// DatasetSynthetic draws the sizes from the scenario's own
	// distributions (the default).
	DatasetSynthetic = "synthetic"
	// DatasetBurstGPT and DatasetAzure take them from a real trace file
	// (Options.BurstGPTPath, Options.AzurePath).
	DatasetBurstGPT = workload.TraceBurstGPT
	DatasetAzure    = workload.TraceAzure
)

// datasetSampleSize bounds the rows kept from a trace file: drawn across
// the whole file, they are far more than the few hundred requests of a run.
const datasetSampleSize = 100_000

// ErrUnknownDataset is returned by StartABWith for a dataset that is not
// one of the Dataset keys or whose file is missing.
var ErrUnknownDataset = errors.New("demo: unknown or unavailable dataset")

// DatasetInfo is one choice of the dashboard's dataset buttons.
type DatasetInfo struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// File is the trace file ("" for DatasetSynthetic) and Available
	// whether the server can read it.
	File      string `json:"file,omitempty"`
	Available bool   `json:"available"`
	Credit    string `json:"credit,omitempty"`
}

// DatasetUse is the dataset of a run (State.Dataset): what was read and
// how its sizes were fitted to the scenario.
type DatasetUse struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	File string `json:"file,omitempty"`
	// Trace counts the file's rows; Sampled is how many of its usable rows
	// the run draws from.
	Trace   *workload.TraceStats `json:"trace,omitempty"`
	Sampled int                  `json:"sampled,omitempty"`
	Fit     *workload.Fit        `json:"fit,omitempty"`
	// Summary says it in a sentence, for the dashboard.
	Summary string `json:"summary"`
}

// loadedDataset is a trace read and fitted once, kept for later runs.
type loadedDataset struct {
	samples []workload.LengthSample
	use     DatasetUse
}

// Datasets lists the dataset choices, with whether each file is there.
func (s *Server) Datasets() []DatasetInfo {
	out := []DatasetInfo{{Key: DatasetSynthetic, Name: "Synthetic", Available: true}}
	for _, d := range []struct{ key, name, path, credit string }{
		{DatasetBurstGPT, "BurstGPT", s.opt.BurstGPTPath, workload.BurstGPTCitation},
		{DatasetAzure, "Azure", s.opt.AzurePath, workload.AzureCitation},
	} {
		info := DatasetInfo{Key: d.key, Name: d.name, Credit: d.credit}
		if d.path != "" {
			st, err := os.Stat(d.path)
			info.File, info.Available = filepath.Base(d.path), err == nil && st.Mode().IsRegular()
		}
		out = append(out, info)
	}
	return out
}

// datasetAvailable reports whether key names an available dataset.
func (s *Server) datasetAvailable(key string) bool {
	for _, d := range s.Datasets() {
		if d.Key == key {
			return d.Available
		}
	}
	return false
}

// datasetPath is the trace file of a dataset key.
func (s *Server) datasetPath(key string) string {
	switch key {
	case DatasetBurstGPT:
		return s.opt.BurstGPTPath
	case DatasetAzure:
		return s.opt.AzurePath
	}
	return ""
}

// datasetSamples returns the fitted request sizes of a trace dataset,
// reading the file on first use (seconds for a large trace) and keeping
// the result for later runs. A failed read is not kept, so a file put in
// place afterwards is used.
func (s *Server) datasetSamples(key string) ([]workload.LengthSample, DatasetUse, error) {
	s.dsMu.Lock()
	defer s.dsMu.Unlock()
	if d, ok := s.loaded[key]; ok {
		return d.samples, d.use, nil
	}
	path := s.datasetPath(key)
	if path == "" {
		return nil, DatasetUse{}, fmt.Errorf("%w: %q", ErrUnknownDataset, key)
	}
	raw, stats, err := workload.SampleTrace(path, key, datasetSampleSize)
	if err != nil {
		return nil, DatasetUse{}, err
	}
	// The real scenarios give every tenant the same length distributions.
	in, out := s.opt.Scenario.Tenants[0].In, s.opt.Scenario.Tenants[0].Out
	fitted, fit := workload.FitSamples(raw, in, out)
	use := DatasetUse{Key: key, Name: datasetName(key), File: filepath.Base(path), Trace: &stats, Sampled: len(raw), Fit: &fit}
	use.Summary = fmt.Sprintf("Request sizes from %s (%s): %d of its %d usable rows, drawn across the whole file. "+
		"To fit this machine, prompts are scaled ×%.3g and answers ×%.3g, so that their averages match this scenario's "+
		"(prompt %.0f, answer %.0f; the dataset's own: %.0f and %.0f tokens); %.1f%% of requests were clamped to the "+
		"scenario's limits (prompt %d-%d, answer %d-%d). Long requests stay long and short ones short. "+
		"Tenants and arrival times stay synthetic (no public trace has tenants).",
		use.Name, use.File, use.Sampled, stats.Usable, fit.InFactor, fit.OutFactor, fit.MeanIn, fit.MeanOut,
		fit.TraceMeanIn, fit.TraceMeanOut, fit.ClampedPct, max(in.Min, 1), in.Max, max(out.Min, 1), out.Max)
	if s.loaded == nil {
		s.loaded = map[string]loadedDataset{}
	}
	s.loaded[key] = loadedDataset{samples: fitted, use: use}
	return fitted, use, nil
}

// syntheticUse is the DatasetUse of a run on the scenario's own sizes.
func syntheticUse() DatasetUse {
	return DatasetUse{Key: DatasetSynthetic, Name: "Synthetic",
		Summary: "Request sizes drawn from the scenario's synthetic distributions (log-normal, sized for this machine)."}
}

func datasetName(key string) string {
	switch key {
	case DatasetBurstGPT:
		return "BurstGPT"
	case DatasetAzure:
		return "Azure LLM inference trace"
	}
	return key
}
