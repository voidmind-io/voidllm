package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// mockLLMServer is a minimal OpenAI-compatible LLM server for benchmarking.
// It returns a fixed chat completion response after a configurable delay.
// When streaming is true, requests with "stream":true receive SSE chunks.
type mockLLMServer struct {
	srv       *http.Server
	addr      string
	latency   time.Duration
	streaming bool
}

// streamingResponse is the hardcoded ~150-token response split into words
// for realistic token-by-token SSE streaming.
var streamingResponse = strings.Fields(
	"Harry Potter is a young orphan living with his cruel aunt and uncle, the Dursleys, " +
		"who treat him as a burden. On his eleventh birthday, he discovers he is a wizard and " +
		"receives an invitation to attend Hogwarts School of Witchcraft and Wizardry. At Hogwarts, " +
		"Harry makes his first real friends, Ron Weasley and Hermione Granger, and learns about the " +
		"magical world his parents belonged to. He discovers that his parents were murdered by the dark " +
		"wizard Lord Voldemort, who also tried to kill Harry as a baby but failed, leaving him with a " +
		"lightning-shaped scar. Throughout the year, Harry and his friends uncover a plot to steal the " +
		"Philosopher's Stone, a powerful artifact hidden within the school. In the final confrontation, " +
		"Harry faces Voldemort, who has been secretly living as a parasite on one of the teachers, and " +
		"manages to protect the Stone through the power of his mother's sacrificial love.",
)

// startMockLLM starts an OpenAI-compatible mock on a random port with non-streaming responses.
func startMockLLM(latency time.Duration) (*mockLLMServer, error) {
	return startMockLLMServer(latency, false)
}

// startMockLLMStreaming starts an OpenAI-compatible mock on a random port with SSE streaming support.
func startMockLLMStreaming(latency time.Duration) (*mockLLMServer, error) {
	return startMockLLMServer(latency, true)
}

// startMockLLMServer starts an OpenAI-compatible mock on a random port.
// When streaming is true, requests containing "stream":true receive SSE chunk responses.
func startMockLLMServer(latency time.Duration, streaming bool) (*mockLLMServer, error) {
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
		if streaming {
			body, _ := io.ReadAll(r.Body)
			if isStreamingRequest(body) {
				writeSSEStream(w, r, latency)
				return
			}
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}

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

	return &mockLLMServer{srv: srv, addr: addr, latency: latency, streaming: streaming}, nil
}

// isStreamingRequest reports whether the request body has "stream":true.
func isStreamingRequest(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Stream
}

// writeSSEStream sends the hardcoded response as OpenAI-compatible SSE chunks.
// It respects client disconnection via r.Context() and emits one word per chunk
// with a 30ms base delay plus 0-20ms random jitter between chunks.
func writeSSEStream(w http.ResponseWriter, r *http.Request, latency time.Duration) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()

	// Apply the configured initial latency (simulates TTFT).
	if latency > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(latency):
		}
	}

	// Role chunk — sent once before content tokens.
	roleChunk := sseChunk(map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "mock",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant"},
			"finish_reason": nil,
		}},
	})
	if _, err := fmt.Fprint(w, roleChunk); err != nil {
		return
	}
	flusher.Flush()

	// Content chunks — one word per chunk.
	for i, word := range streamingResponse {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Space-prefix all words except the first so the joined text reads naturally.
		content := word
		if i > 0 {
			content = " " + word
		}

		chunk := sseChunk(map[string]any{
			"id":      "chatcmpl-bench",
			"object":  "chat.completion.chunk",
			"created": 1700000000,
			"model":   "mock",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": content},
				"finish_reason": nil,
			}},
		})
		if _, err := fmt.Fprint(w, chunk); err != nil {
			return
		}
		flusher.Flush()

		// 30ms base + 0-20ms jitter.
		jitter := time.Duration(rand.Intn(21)) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(30*time.Millisecond + jitter):
		}
	}

	// Final chunk with finish_reason and usage.
	stopChunk := sseChunk(map[string]any{
		"id":      "chatcmpl-bench",
		"object":  "chat.completion.chunk",
		"created": 1700000000,
		"model":   "mock",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     25,
			"completion_tokens": 150,
			"total_tokens":      175,
		},
	})
	_, _ = fmt.Fprint(w, stopChunk)
	flusher.Flush()

	// Termination sentinel.
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// sseChunk marshals v to JSON and wraps it in the SSE "data: ...\n\n" envelope.
func sseChunk(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// Close shuts down the mock server with a short timeout.
func (m *mockLLMServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.srv.Shutdown(ctx)
}

// URL returns the base URL of the mock server.
func (m *mockLLMServer) URL() string {
	return m.addr
}
