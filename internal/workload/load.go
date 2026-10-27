package workload

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
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
	burstGPTFormat = traceFormat{
		name: "BurstGPT",
		header: []string{"Timestamp", "Session ID", "Elapsed time", "Model",
			"Request tokens", "Response tokens", "Total tokens", "Log Type"},
		inCol: 4, outCol: 5,
		skipZeroOut: true,
	}
	azureFormat = traceFormat{
		name:   "Azure LLM inference",
		header: []string{"TIMESTAMP", "ContextTokens", "GeneratedTokens"},
		inCol:  1, outCol: 2,
	}
)

// LoadBurstGPT reads token lengths from a BurstGPT CSV file with the header
// Timestamp,Session ID,Elapsed time,Model,Request tokens,Response tokens,Total tokens,Log Type.
// In is "Request tokens" and Out is "Response tokens". Rows with zero
// response tokens are failed requests and are skipped.
func LoadBurstGPT(path string) ([]LengthSample, error) {
	return loadTrace(path, burstGPTFormat)
}

// LoadAzure reads token lengths from an Azure LLM Inference Trace 2024 CSV
// file with the header TIMESTAMP,ContextTokens,GeneratedTokens. In is
// ContextTokens and Out is GeneratedTokens. Every row is kept, including
// rows with zero generated tokens (their prompts are still real load);
// Generate replays those as one-token requests.
func LoadAzure(path string) ([]LengthSample, error) {
	return loadTrace(path, azureFormat)
}

func loadTrace(path string, f traceFormat) ([]LengthSample, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s trace: %w", f.name, err)
	}
	defer file.Close()
	samples, err := parseTrace(bufio.NewReader(file), f)
	if err != nil {
		return nil, fmt.Errorf("%s trace %s: %w", f.name, path, err)
	}
	return samples, nil
}

func parseTrace(r io.Reader, f traceFormat) ([]LengthSample, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // the header is checked by checkHeader
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("file is empty; want header %q", strings.Join(f.header, ","))
	}
	if err != nil {
		return nil, fmt.Errorf("read header (want %q): %w", strings.Join(f.header, ","), err)
	}
	if err := checkHeader(header, f.header); err != nil {
		return nil, err
	}
	cr.FieldsPerRecord = len(f.header)
	var samples []LengthSample
	rows := 0
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		rows++
		line, _ := cr.FieldPos(0)
		in, err := tokenCount(rec[f.inCol], f.header[f.inCol], line)
		if err != nil {
			return nil, err
		}
		out, err := tokenCount(rec[f.outCol], f.header[f.outCol], line)
		if err != nil {
			return nil, err
		}
		if f.skipZeroOut && out == 0 {
			continue
		}
		samples = append(samples, LengthSample{In: in, Out: out})
	}
	if len(samples) == 0 {
		msg := fmt.Sprintf("no usable rows: %d data rows", rows)
		if rows > 0 {
			msg += fmt.Sprintf(", none with %s > 0", f.header[f.outCol])
		}
		return nil, errors.New(msg)
	}
	return samples, nil
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
