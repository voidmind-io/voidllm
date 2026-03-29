// Mock LLM server for benchmarking proxy overhead.
// Returns a fixed chat completion response with configurable latency.
// Usage: go run scripts/bench/mock-llm.go [-latency 10ms] [-port 9999]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var response = map[string]any{
	"id":      "chatcmpl-bench",
	"object":  "chat.completion",
	"created": 1700000000,
	"model":   "mock",
	"choices": []map[string]any{
		{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "Hello from the mock LLM.",
			},
			"finish_reason": "stop",
		},
	},
	"usage": map[string]any{
		"prompt_tokens":     10,
		"completion_tokens": 8,
		"total_tokens":      18,
	},
}

func main() {
	latency := flag.Duration("latency", 10*time.Millisecond, "simulated response latency")
	port := flag.Int("port", 9999, "listen port")
	flag.Parse()

	responseBytes, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling response: %v\n", err)
		os.Exit(1)
	}

	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if *latency > 0 {
			time.Sleep(*latency)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(responseBytes)
	})

	http.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"mock","object":"model","created":1700000000,"owned_by":"bench"}]}`)
	})

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("Mock LLM listening on %s (latency: %v)\n", addr, *latency)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
