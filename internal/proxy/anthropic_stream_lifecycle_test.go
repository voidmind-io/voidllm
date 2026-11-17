package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

const (
	lcMessageStart = `data: {"type":"message_start","message":{"id":"msg_lc","type":"message","role":"assistant","content":[],"model":"claude-test","usage":{"input_tokens":11,"output_tokens":0}}}`
	lcBlockStart   = `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	lcBlockDelta   = `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`
	lcBlockStop    = `data: {"type":"content_block_stop","index":0}`
	lcMessageDelta = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`
	lcMessageStop  = `data: {"type":"message_stop"}`
	lcPing         = `data: {"type":"ping"}`
)

func TestAnthropicStreamLifecycle_FirstEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantAbort bool
	}{
		{"content_block_start first aborts", lcBlockStart, true},
		{"content_block_delta first aborts", lcBlockDelta, true},
		{"message_delta first aborts", lcMessageDelta, true},
		{"message_stop first aborts", lcMessageStop, true},
		{"ping first does not abort", lcPing, false},
		{"content_block_stop first does not abort", lcBlockStop, false},
		{"unknown event first does not abort", `data: {"type":"brand_new_event"}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformStreamLine([]byte(tc.line))
			if tc.wantAbort {
				if !errors.Is(err, errStreamTransformAborted) {
					t.Fatalf("err = %v, want errStreamTransformAborted", err)
				}
				if len(out) != 0 {
					t.Errorf("output = %q, want none on abort", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAnthropicStreamLifecycle_MessageStartViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
	}{
		{"duplicate message_start aborts", []string{lcMessageStart, lcMessageStart}},
		{"message_start with non-object message aborts", []string{`data: {"type":"message_start","message":"oops"}`}},
		{"non-JSON message_start passes through then next lifecycle event aborts", []string{`data: {"type":"message_start","message":`, lcBlockDelta}},
		{"message_start with wrong field types aborts", []string{`data: {"type":"message_start","message":{"usage":{"input_tokens":"many"}}}`}},
		{"events after malformed message_start still abort", []string{`data: {"type":"message_start","message":{"usage":{"input_tokens":"many"}}}`, lcBlockDelta}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			var lastErr error
			var lastOut [][]byte
			for _, l := range tc.lines {
				lastOut, lastErr = a.TransformStreamLine([]byte(l))
				if lastErr != nil {
					break
				}
			}
			if !errors.Is(lastErr, errStreamTransformAborted) {
				t.Fatalf("err = %v, want errStreamTransformAborted", lastErr)
			}
			if len(lastOut) != 0 {
				t.Errorf("output = %q, want none on abort", lastOut)
			}
		})
	}
}

func TestAnthropicStreamLifecycle_PingThenNormalStream(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	if _, err := a.TransformStreamLine([]byte(lcPing)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	out, err := a.TransformStreamLine([]byte(lcMessageStart))
	if err != nil {
		t.Fatalf("message_start after ping: %v", err)
	}
	if len(out) != 1 || !strings.Contains(string(out[0]), `"role":"assistant"`) {
		t.Errorf("message_start output = %q, want one role chunk", out)
	}
	out, err = a.TransformStreamLine([]byte(lcBlockDelta))
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(out) != 1 || !strings.Contains(string(out[0]), `"content":"hello"`) {
		t.Errorf("delta output = %q, want content chunk", out)
	}
}

func TestAnthropicStreamLifecycle_NormalStream(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	var all []string
	for _, l := range []string{lcMessageStart, lcBlockStart, lcBlockDelta, lcBlockStop, lcMessageDelta, lcMessageStop} {
		out, err := a.TransformStreamLine([]byte(l))
		if err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		for _, o := range out {
			all = append(all, string(o))
		}
	}
	joined := strings.Join(all, "")
	for _, want := range []string{`"role":"assistant"`, `"content":"hello"`, `"finish_reason":"stop"`, "[DONE]", "chatcmpl-proxy"} {
		if want == "chatcmpl-proxy" {
			// message id from message_start must be used instead.
			if strings.Contains(joined, want) {
				t.Errorf("output uses fallback id, want msg_lc\n%s", joined)
			}
			continue
		}
		if !strings.Contains(joined, want) {
			t.Errorf("output missing %q\n%s", want, joined)
		}
	}
	u := a.StreamUsage()
	if u.PromptTokens != 11 || u.CompletionTokens != 7 || u.TotalTokens != 18 {
		t.Errorf("StreamUsage = %+v, want prompt 11 completion 7 total 18", u)
	}
}

// lifecycleUpstream serves the given SSE data lines as an Anthropic stream.
func lifecycleUpstream(t *testing.T, lines []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			fmt.Fprintf(w, "%s\n\n", l)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitForLoggedUsage polls usage_events for the first row and returns its
// status code and prompt tokens.
func waitForLoggedUsage(t *testing.T, d *db.DB) (status, promptTokens int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := d.SQL().QueryRowContext(context.Background(),
			"SELECT status_code, prompt_tokens FROM usage_events ORDER BY rowid DESC LIMIT 1").
			Scan(&status, &promptTokens)
		if err == nil {
			return status, promptTokens
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("usage event was not logged within deadline")
	return 0, 0
}

func TestHandle_AnthropicStreamLifecycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		lines        []string
		wantStatus   int
		wantAbort    bool
		wantPromptGT int
	}{
		{
			name:       "missing message_start is recorded as 502",
			lines:      []string{lcBlockStart, lcBlockDelta, lcBlockStop, lcMessageDelta, lcMessageStop},
			wantStatus: http.StatusBadGateway,
			wantAbort:  true,
		},
		{
			name:         "normal stream is recorded as 200 with prompt tokens",
			lines:        []string{lcMessageStart, lcBlockStart, lcBlockDelta, lcBlockStop, lcMessageDelta, lcMessageStop},
			wantStatus:   http.StatusOK,
			wantPromptGT: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := lifecycleUpstream(t, tc.lines)
			reg, err := NewRegistry([]config.ModelConfig{{
				Name:     "lc-anthropic",
				Provider: "anthropic",
				BaseURL:  upstream.URL,
				APIKey:   "upstream-secret",
			}})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			h, _, d := newTerminationTestHandler(t, reg)
			baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-lc"))
			body := doStreamRequest(t, baseURL, rawKey, "lc-anthropic")

			if tc.wantAbort {
				if n := countSegmentsContaining(body, "stream_transform_aborted"); n != 1 {
					t.Errorf("abort segments = %d, want 1\noutput: %s", n, body)
				}
				if strings.Contains(body, "[DONE]") {
					t.Errorf("[DONE] present, want absent\noutput: %s", body)
				}
			} else {
				if strings.Contains(body, "stream_transform_aborted") {
					t.Errorf("unexpected abort event\noutput: %s", body)
				}
				if !strings.Contains(body, "[DONE]") {
					t.Errorf("[DONE] absent\noutput: %s", body)
				}
			}

			status, prompt := waitForLoggedUsage(t, d)
			if status != tc.wantStatus {
				t.Errorf("usage status_code = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && prompt <= tc.wantPromptGT {
				t.Errorf("prompt_tokens = %d, want > %d", prompt, tc.wantPromptGT)
			}
		})
	}
}
