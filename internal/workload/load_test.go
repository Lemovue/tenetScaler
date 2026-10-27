package workload

import (
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
