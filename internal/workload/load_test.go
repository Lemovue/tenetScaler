package workload

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const burstGPTHeaderLine = "Timestamp,Session ID,Elapsed time,Model,Request tokens,Response tokens,Total tokens,Log Type\n"

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBurstGPT(t *testing.T) {
	content := "\xef\xbb\xbf" + strings.Replace(burstGPTHeaderLine, "Model", " Model ", 1) +
		"5,1,0.81,ChatGPT,472,18,490,Conversation log\n" +
		"6,2,1.20,GPT-4,1024,0,1024,Conversation log\n" + // failed request: skipped
		"8,3,0.40,ChatGPT, 30 ,256,286,API log\n" +
		"9,4,0.10,ChatGPT,0,5,5,API log\n"
	got, err := LoadBurstGPT(writeCSV(t, content))
	if err != nil {
		t.Fatal(err)
	}
	want := []LengthSample{{In: 472, Out: 18}, {In: 30, Out: 256}, {In: 0, Out: 5}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadBurstGPT = %v, want %v", got, want)
	}
}

// The release files (e.g. BurstGPT_1.csv of release v2.0) have no Session
// ID and Elapsed time columns.
func TestLoadBurstGPTReleaseLayout(t *testing.T) {
	content := "Timestamp,Model,Request tokens,Response tokens,Total tokens,Log Type\n" +
		"5,ChatGPT,472,18,490,Conversation log\n" +
		"45,ChatGPT,1087,0,1087,Conversation log\n" + // failed request: skipped
		"118,GPT-4,417,276,693,Conversation log\n"
	got, err := LoadBurstGPT(writeCSV(t, content))
	if err != nil {
		t.Fatal(err)
	}
	want := []LengthSample{{In: 472, Out: 18}, {In: 417, Out: 276}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadBurstGPT = %v, want %v", got, want)
	}
}

// SampleTrace keeps at most n usable rows, the same ones on every read,
// counts the file's rows, and refuses a file of the other kind.
func TestSampleTrace(t *testing.T) {
	var b strings.Builder
	b.WriteString("Timestamp,Model,Request tokens,Response tokens,Total tokens,Log Type\n")
	for i := range 1000 {
		out := i % 7 // every 7th row (out 0) is a failure
		fmt.Fprintf(&b, "%d,ChatGPT,%d,%d,%d,API log\n", i, i+1, out, i+1+out)
	}
	path := writeCSV(t, b.String())
	got, stats, err := SampleTrace(path, TraceBurstGPT, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 || stats.Rows != 1000 || stats.Usable != 1000-143 {
		t.Fatalf("got %d samples, stats %+v; want 50 of 857 usable in 1000 rows", len(got), stats)
	}
	again, _, err := SampleTrace(path, TraceBurstGPT, 50)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("a second read gave other samples (%v)", err)
	}
	late := 0
	for _, s := range got {
		if s.Out == 0 {
			t.Fatalf("a failed row was kept: %+v", s)
		}
		if s.In > 500 {
			late++
		}
	}
	if late == 0 || late == len(got) {
		t.Errorf("%d of %d samples from the second half of the file: want rows from the whole file", late, len(got))
	}
	all, _, err := SampleTrace(path, TraceBurstGPT, 0)
	if err != nil || len(all) != 857 {
		t.Fatalf("n = 0 kept %d rows (%v), want all 857", len(all), err)
	}
	if _, _, err := SampleTrace(path, TraceAzure, 50); err == nil || !strings.Contains(err.Error(), "unexpected header") {
		t.Errorf("a BurstGPT file read as Azure: %v, want a header error", err)
	}
	if _, _, err := SampleTrace(path, "other", 50); err == nil {
		t.Error("an unknown trace kind was accepted")
	}
}

func TestLoadAzure(t *testing.T) {
	content := "TIMESTAMP,ContextTokens,GeneratedTokens\n" +
		"2024-05-10 00:00:00.009930+00:00,2048,128\n" +
		"2024-05-10 00:00:00.015000+00:00,374,0\n" + // zero output: kept (only BurstGPT zeros are failures)
		"2024-05-10 00:00:01.000000+00:00,17,44\n"
	got, err := LoadAzure(writeCSV(t, content))
	if err != nil {
		t.Fatal(err)
	}
	want := []LengthSample{{In: 2048, Out: 128}, {In: 374, Out: 0}, {In: 17, Out: 44}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadAzure = %v, want %v", got, want)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		load    func(string) ([]LengthSample, error)
		content string
		want    []string
	}{
		{"empty file", LoadBurstGPT, "", []string{"file is empty", "Request tokens"}},
		{"azure header given to burstgpt", LoadBurstGPT, "TIMESTAMP,ContextTokens,GeneratedTokens\n1,2,3\n",
			[]string{"unexpected header", "3 columns, want 8", "Timestamp,Session ID"}},
		{"renamed column", LoadBurstGPT, strings.Replace(burstGPTHeaderLine, "Response tokens", "Output tokens", 1),
			[]string{"column 6 is \"Output tokens\", want \"Response tokens\""}},
		{"wrong case", LoadAzure, "timestamp,ContextTokens,GeneratedTokens\n", []string{"column 1 is \"timestamp\""}},
		{"not an integer", LoadBurstGPT, burstGPTHeaderLine + "1,1,0.5,ChatGPT,12,4,16,API log\n2,1,0.5,ChatGPT,abc,4,16,API log\n",
			[]string{"line 3", "Request tokens \"abc\" is not an integer"}},
		{"negative", LoadAzure, "TIMESTAMP,ContextTokens,GeneratedTokens\nt,5,-2\n", []string{"line 2", "GeneratedTokens is negative"}},
		{"ragged row", LoadAzure, "TIMESTAMP,ContextTokens,GeneratedTokens\nt,5\n", []string{"wrong number of fields"}},
		{"only failed rows", LoadBurstGPT, burstGPTHeaderLine + "1,1,0.5,ChatGPT,12,0,12,API log\n",
			[]string{"no usable rows", "1 data rows", "Response tokens > 0"}},
		{"header only", LoadAzure, "TIMESTAMP,ContextTokens,GeneratedTokens\n", []string{"no usable rows: 0 data rows"}},
		{"burstgpt header only", LoadBurstGPT, burstGPTHeaderLine, []string{"no usable rows: 0 data rows"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCSV(t, tc.content)
			_, err := tc.load(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the file", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := LoadAzure(filepath.Join(t.TempDir(), "missing.csv"))
	if err == nil || !strings.Contains(err.Error(), "open Azure LLM inference trace") {
		t.Fatalf("LoadAzure(missing) = %v, want an open error", err)
	}
}
