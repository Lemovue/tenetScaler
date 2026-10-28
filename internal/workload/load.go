package workload

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
)

// traceFormat describes one supported trace CSV layout.
type traceFormat struct {
	name          string
	header        []string
	inCol, outCol int
	// skipZeroOut drops rows with zero output tokens: in BurstGPT those
	// are failed requests.
	skipZeroOut bool
}

var (
	// burstGPTFormats are BurstGPT's two layouts: with the Session ID and
	// Elapsed time columns, and without them, as in the release files
	// (github.com/HPMLL/BurstGPT releases v1.0 to v2.0, e.g. BurstGPT_1.csv).
	burstGPTFormats = []traceFormat{
		{
			name: "BurstGPT",
			header: []string{"Timestamp", "Session ID", "Elapsed time", "Model",
				"Request tokens", "Response tokens", "Total tokens", "Log Type"},
			inCol: 4, outCol: 5,
			skipZeroOut: true,
		},
		{
			name:   "BurstGPT",
			header: []string{"Timestamp", "Model", "Request tokens", "Response tokens", "Total tokens", "Log Type"},
			inCol:  2, outCol: 3,
			skipZeroOut: true,
		},
	}
	azureFormat = traceFormat{
		name:   "Azure LLM inference",
		header: []string{"TIMESTAMP", "ContextTokens", "GeneratedTokens"},
		inCol:  1, outCol: 2,
	}
)

// Trace kinds accepted by SampleTrace.
const (
	TraceBurstGPT = "burstgpt"
	TraceAzure    = "azure"
)

// Credits of the traces (both CC-BY).
const (
	BurstGPTCitation = "BurstGPT, github.com/HPMLL/BurstGPT (CC-BY-4.0); Wang et al., \"BurstGPT: A Real-World Workload " +
		"Dataset to Optimize LLM Serving Systems\", KDD '25"
	AzureCitation = "Azure LLM inference trace 2024, github.com/Azure/AzurePublicDataset (CC-BY); Stojkovic et al., " +
		"\"DynamoLLM: Designing LLM Inference Clusters for Performance and Energy Efficiency\", HPCA 2025"
)

// LoadBurstGPT reads token lengths from a BurstGPT CSV file with the header
// Timestamp,Session ID,Elapsed time,Model,Request tokens,Response tokens,Total tokens,Log Type
// or, as in the release files, Timestamp,Model,Request tokens,Response tokens,Total tokens,Log Type.
// In is "Request tokens" and Out is "Response tokens". Rows with zero
// response tokens are failed requests and are skipped.
func LoadBurstGPT(path string) ([]LengthSample, error) {
	samples, _, err := loadTrace(path, burstGPTFormats, 0)
	return samples, err
}

// LoadAzure reads token lengths from an Azure LLM Inference Trace 2024 CSV
// file with the header TIMESTAMP,ContextTokens,GeneratedTokens. In is
// ContextTokens and Out is GeneratedTokens. Every row is kept, including
// rows with zero generated tokens (their prompts are still real load);
// Generate replays those as one-token requests.
func LoadAzure(path string) ([]LengthSample, error) {
	samples, _, err := loadTrace(path, []traceFormat{azureFormat}, 0)
	return samples, err
}

// TraceStats describes the file SampleTrace read.
type TraceStats struct {
	// Rows counts the data rows and Usable those kept (BurstGPT drops its
	// failed requests).
	Rows   int `json:"rows"`
	Usable int `json:"usable"`
}

// SampleTrace reads at most n token-length pairs from a trace of the given
// kind (TraceBurstGPT or TraceAzure, read as LoadBurstGPT and LoadAzure
// do), drawn uniformly over the whole file by reservoir sampling with a
// fixed seed: the same file always gives the same samples, and a trace of
// millions of rows costs n pairs of memory. n <= 0 keeps every usable row.
func SampleTrace(path, kind string, n int) ([]LengthSample, TraceStats, error) {
	switch kind {
	case TraceBurstGPT:
		return loadTrace(path, burstGPTFormats, n)
	case TraceAzure:
		return loadTrace(path, []traceFormat{azureFormat}, n)
	}
	return nil, TraceStats{}, fmt.Errorf("trace kind must be %q or %q, got %q", TraceBurstGPT, TraceAzure, kind)
}

func loadTrace(path string, formats []traceFormat, n int) ([]LengthSample, TraceStats, error) {
	name := formats[0].name
	file, err := os.Open(path)
	if err != nil {
		return nil, TraceStats{}, fmt.Errorf("open %s trace: %w", name, err)
	}
	defer file.Close()
	samples, stats, err := parseTrace(bufio.NewReader(file), formats, n)
	if err != nil {
		return nil, stats, fmt.Errorf("%s trace %s: %w", name, path, err)
	}
	return samples, stats, nil
}

// parseTrace reads a trace in the first of formats whose header matches,
// keeping at most n usable rows (n <= 0: all) by reservoir sampling.
func parseTrace(r io.Reader, formats []traceFormat, n int) ([]LengthSample, TraceStats, error) {
	var stats TraceStats
	want := formats[0].header
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // the header is checked by checkHeader
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, stats, fmt.Errorf("file is empty; want header %q", strings.Join(want, ","))
	}
	if err != nil {
		return nil, stats, fmt.Errorf("read header (want %q): %w", strings.Join(want, ","), err)
	}
	f, err := matchFormat(header, formats)
	if err != nil {
		return nil, stats, err
	}
	cr.FieldsPerRecord = len(f.header)
	var samples []LengthSample
	reservoir := rand.New(rand.NewPCG(1, 2))
	rows := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, stats, err
		}
		rows++
		line, _ := cr.FieldPos(0)
		in, err := tokenCount(rec[f.inCol], f.header[f.inCol], line)
		if err != nil {
			return nil, stats, err
		}
		out, err := tokenCount(rec[f.outCol], f.header[f.outCol], line)
		if err != nil {
			return nil, stats, err
		}
		if f.skipZeroOut && out == 0 {
			continue
		}
		stats.Usable++
		smp := LengthSample{In: in, Out: out}
		switch {
		case n <= 0 || len(samples) < n:
			samples = append(samples, smp)
		default: // keep the row with probability n / Usable
			if j := reservoir.IntN(stats.Usable); j < n {
				samples[j] = smp
			}
		}
	}
	stats.Rows = rows
	if len(samples) == 0 {
		msg := fmt.Sprintf("no usable rows: %d data rows", rows)
		if rows > 0 {
			msg += fmt.Sprintf(", none with %s > 0", f.header[f.outCol])
		}
		return nil, stats, errors.New(msg)
	}
	return samples, stats, nil
}

// matchFormat returns the first of formats whose header got is; when none
// matches, the error explains the mismatch with the first.
func matchFormat(got []string, formats []traceFormat) (traceFormat, error) {
	for _, f := range formats {
		if checkHeader(got, f.header) == nil {
			return f, nil
		}
	}
	err := checkHeader(got, formats[0].header)
	for _, f := range formats[1:] {
		err = fmt.Errorf("%w; or the header %q", err, strings.Join(f.header, ","))
	}
	return traceFormat{}, err
}

// byteOrderMark is U+FEFF, which some tools write at the start of a CSV.
const byteOrderMark = "\xef\xbb\xbf"

// checkHeader requires the exact expected columns, tolerating surrounding
// spaces and a UTF-8 byte-order mark.
func checkHeader(got, want []string) error {
	clean := make([]string, len(got))
	for i, h := range got {
		clean[i] = strings.TrimSpace(strings.TrimPrefix(h, byteOrderMark))
	}
	if len(clean) != len(want) {
		return fmt.Errorf("unexpected header %q: %d columns, want %d (expected header %q)",
			strings.Join(clean, ","), len(clean), len(want), strings.Join(want, ","))
	}
	for i := range want {
		if clean[i] != want[i] {
			return fmt.Errorf("unexpected header %q: column %d is %q, want %q (expected header %q)",
				strings.Join(clean, ","), i+1, clean[i], want[i], strings.Join(want, ","))
		}
	}
	return nil
}

func tokenCount(field, column string, line int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(field))
	if err != nil {
		return 0, fmt.Errorf("line %d: %s %q is not an integer", line, column, field)
	}
	if n < 0 {
		return 0, fmt.Errorf("line %d: %s is negative (%d)", line, column, n)
	}
	return n, nil
}
