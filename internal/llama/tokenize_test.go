package llama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Content string `json:"content"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/tokenize" || json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// A stand-in tokenizer: one token per character.
		toks := make([]int, len(in.Content))
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
	}))
	defer srv.Close()
	count := TokenCounter()
	if n, err := count(context.Background(), srv.URL+"/", "hello"); err != nil || n != 5 {
		t.Fatalf("count = %d, %v; want 5", n, err)
	}
	if _, err := count(context.Background(), srv.URL+"/nope", "x"); err == nil || !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "404") {
		t.Fatalf("bad endpoint: %v", err)
	}
}
