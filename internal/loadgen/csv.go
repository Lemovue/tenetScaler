package loadgen

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// RecordsCSVHeader is the header row WriteRecordsCSV writes. Times are
// seconds from the run start (wall clock); ttft_s and tpot_s are empty for
// requests that did not complete. simulated is true on every row of a
// target whose responses came from a simulated replica
// (Result.Simulated): its latencies are modelled, not real inference.
var RecordsCSVHeader = []string{
	"target", "id", "tenant", "priority", "arrival_s", "first_token_s", "done_s",
	"ttft_s", "tpot_s", "shed", "shed_reason", "failed", "input_tokens", "output_tokens",
	"simulated",
}

// WriteRecordsCSV writes the records of every target of res (targets in
// name order, records in the given order) as CSV with RecordsCSVHeader.
func WriteRecordsCSV(w io.Writer, res Result) error {
	byTarget := res.Records
	cw := csv.NewWriter(w)
	if err := cw.Write(RecordsCSVHeader); err != nil {
		return fmt.Errorf("write records csv: %w", err)
	}
	names := make([]string, 0, len(byTarget))
	for n := range byTarget {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		simulated := strconv.FormatBool(res.Simulated(n))
		for _, r := range byTarget[n] {
			ttft, tpot := "", ""
			if r.Completed() {
				ttft, tpot = secs(r.TTFT()), secs(r.TPOT())
			}
			row := []string{
				n, strconv.FormatUint(r.ID, 10), r.Tenant, r.Priority.Class(),
				secs(r.Arrival), secs(r.FirstToken), secs(r.Done), ttft, tpot,
				strconv.FormatBool(r.Shed), r.ShedReason, strconv.FormatBool(r.Failed),
				strconv.Itoa(r.InputTokens), strconv.Itoa(r.OutputTokens), simulated,
			}
			if err := cw.Write(row); err != nil {
				return fmt.Errorf("write records csv: %w", err)
			}
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("write records csv: %w", err)
	}
	return nil
}

// secs formats a duration as seconds with microsecond precision.
func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 6, 64) }
