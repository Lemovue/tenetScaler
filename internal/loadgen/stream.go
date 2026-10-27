package loadgen

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// maxSSELine bounds one SSE line (a llama-server final chunk with timings
// is well under 4 KiB).
const maxSSELine = 1 << 20

// streamChunk is the part of an OpenAI-style streamed completion chunk the
// generator reads. It matches both the simulated replica and llama-server.
type streamChunk struct {
	Choices []struct {
		Text         string  `json:"text"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// streamResult is what readStream measured.
type streamResult struct {
	// firstToken is the offset of the first chunk with text, done the
	// offset of "data: [DONE]".
	firstToken, done time.Duration
	// textChunks counts chunks with text; promptTokens and
	// completionTokens are the final usage.prompt_tokens and
	// usage.completion_tokens (0 when absent).
	textChunks, promptTokens, completionTokens int
}

// readStream consumes an SSE completion stream until "data: [DONE]",
// timestamping with now. A stream that ends (EOF) after a chunk with a
// finish_reason also counts as complete, for servers that omit [DONE]. A
// stream that ends earlier, carries an error object or an "error:" event,
// or holds an unparsable chunk is an error.
func readStream(r io.Reader, now func() time.Duration) (streamResult, error) {
	var res streamResult
	finished := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxSSELine)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, []byte("error:")) {
			return res, fmt.Errorf("server error event: %s", bytes.TrimSpace(line[len("error:"):]))
		}
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue // blank separator, comment or another SSE field
		}
		payload = bytes.TrimSpace(payload)
		if string(payload) == "[DONE]" {
			res.done = now()
			return res, nil
		}
		var c streamChunk
		if err := json.Unmarshal(payload, &c); err != nil {
			return res, fmt.Errorf("decode chunk: %w", err)
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			return res, fmt.Errorf("server error in stream: %s", c.Error)
		}
		for _, ch := range c.Choices {
			if ch.Text != "" {
				if res.textChunks == 0 {
					res.firstToken = now()
				}
				res.textChunks++
				break
			}
		}
		for _, ch := range c.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finished = true
			}
		}
		if c.Usage != nil {
			if c.Usage.PromptTokens > 0 {
				res.promptTokens = c.Usage.PromptTokens
			}
			if c.Usage.CompletionTokens > 0 {
				res.completionTokens = c.Usage.CompletionTokens
			}
		}
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("read stream: %w", err)
	}
	if finished {
		res.done = now()
		return res, nil
	}
	return res, errors.New("stream ended before [DONE]")
}
