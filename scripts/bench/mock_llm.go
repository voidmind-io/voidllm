package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// mockLLMServer is a minimal OpenAI-compatible LLM server for benchmarking.
// It returns a fixed chat completion response after a configurable delay.
type mockLLMServer struct {
	srv     *http.Server
	addr    string
	latency time.Duration
}

// startMockLLM starts an OpenAI-compatible mock on a random port.
func startMockLLM(latency time.Duration) (*mockLLMServer, error) {
	response, _ := json.Marshal(map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "mock",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "Hello from the mock LLM."},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		if latency > 0 {
			time.Sleep(latency)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"mock","object":"model","created":1700000000,"owned_by":"bench"}]}`)
	})

	// Large payload variant — generates a response proportional to input.
	mux.HandleFunc("/v1/chat/completions/large", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		if latency > 0 {
			time.Sleep(latency)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock llm: listen: %w", err)
	}

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	addr := ln.Addr().String()
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}

	return &mockLLMServer{srv: srv, addr: addr, latency: latency}, nil
}

func (m *mockLLMServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.srv.Shutdown(ctx)
}

func (m *mockLLMServer) URL() string {
	return m.addr
}
