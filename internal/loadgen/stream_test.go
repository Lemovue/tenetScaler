package loadgen

import (
	"strings"
	"testing"
	"time"
)

// ticker returns a clock that advances by one millisecond per call.
func ticker() func() time.Duration {
	var n time.Duration
	return func() time.Duration { n += time.Millisecond; return n }
}

func TestReadStream(t *testing.T) {
	const llamaFinal = `data: {"choices":[{"text":"","index":0,"finish_reason":"length"}],"usage":{"completion_tokens":2,"prompt_tokens":4,"total_tokens":6},"timings":{"predicted_n":2}}`
	tests := []struct {
		name    string
		stream  string
		want    streamResult
		wantErr string
	}{
		{
			name: "llama-server shape",
			stream: ": keep-alive\n\n" +
				"data: {\"choices\":[{\"text\":\"\",\"index\":0,\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"text\":\" sky\",\"index\":0,\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"text\":\" blue\",\"index\":0,\"finish_reason\":null}]}\n\n" +
				llamaFinal + "\n\ndata: [DONE]\n\n",
			want: streamResult{firstToken: time.Millisecond, done: 2 * time.Millisecond, textChunks: 2, promptTokens: 4, completionTokens: 2},
		},
		{
			name:   "CRLF lines, text in the final chunk, no usage",
			stream: "data: {\"choices\":[{\"text\":\"a\",\"finish_reason\":\"length\"}]}\r\n\r\ndata: [DONE]\r\n\r\n",
			want:   streamResult{firstToken: time.Millisecond, done: 2 * time.Millisecond, textChunks: 1},
		},
		{
			name:   "EOF after finish_reason",
			stream: "data: {\"choices\":[{\"text\":\"a\",\"finish_reason\":\"length\"}]}\n\n",
			want:   streamResult{firstToken: time.Millisecond, done: 2 * time.Millisecond, textChunks: 1},
		},
		{name: "EOF before finish", stream: "data: {\"choices\":[{\"text\":\"a\"}]}\n\n", wantErr: "before [DONE]"},
		{name: "error object", stream: "data: {\"error\":{\"message\":\"context full\"}}\n\n", wantErr: "context full"},
		{name: "error event", stream: "error: {\"message\":\"x\"}\n\n", wantErr: "error event"},
		{name: "bad chunk", stream: "data: {nope\n\n", wantErr: "decode chunk"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readStream(strings.NewReader(tc.stream), ticker())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
