package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
)

// closingUpstream is a raw TCP replica that answers the first request on
// its first connection with keep-alive, then reads the next request on that
// connection and closes it without answering: the race llama-server's
// keep-alive timeout produces when the gateway reuses an idle connection.
// Every later connection is served normally.
func closingUpstream(t *testing.T) (url string, conns *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	conns = &atomic.Int32{}
	const body = `{"choices":[{"index":0,"text":"tok tok","finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			first := conns.Add(1) == 1
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for n := 1; ; n++ {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					if first && n == 2 {
						return // closed with the request unanswered
					}
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				}
			}()
		}
	}()
	return "http://" + ln.Addr().String(), conns
}

// TestReplicaClosingIdleConnection: a request sent on a reused connection
// that the replica closes before answering is resent on a new connection,
// not failed with 502.
func TestReplicaClosingIdleConnection(t *testing.T) {
	url, conns := closingUpstream(t)
	_, base := startGateway(t, Config{Discovery: StaticDiscovery{url}})
	for i := range 2 {
		if r := post(t, base, "A", "", completionBody("w", 2, false)); r.status != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200: %s", i+1, r.status, r.body)
		}
	}
	if n := conns.Load(); n != 2 {
		t.Fatalf("replica saw %d connections, want 2 (the second request reused the first, then was resent)", n)
	}
}
