package proxy

// bedrock_zero_usage_test.go covers the zero-input-token usage anomaly: the
// BedrockAdapter flag (ZeroInputUsageReported) and the handler WARN record that
// is emitted in both the streaming and buffered paths without altering client
// responses or usage events.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

const zuWarnMsg = "upstream reported zero input tokens"

func TestBedrockAdapter_StreamZeroInputUsageReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		meta       string
		want       bool
		wantPrompt float64
		wantCompl  float64
	}{
		{
			name: "all zero usage",
			meta: `{"metadata":{"usage":{"inputTokens":0,"outputTokens":0,"totalTokens":0}}}`,
			want: true,
		},
		{
			name:      "zero input with output tokens",
			meta:      `{"metadata":{"usage":{"inputTokens":0,"outputTokens":7,"totalTokens":7}}}`,
			want:      true,
			wantCompl: 7,
		},
		{
			name:       "normal usage",
			meta:       bsMeta,
			want:       false,
			wantPrompt: 10,
			wantCompl:  5,
		},
		{
			name:       "zero input but cache read",
			meta:       `{"metadata":{"usage":{"inputTokens":0,"outputTokens":5,"cacheReadInputTokens":40}}}`,
			want:       false,
			wantPrompt: 40,
			wantCompl:  5,
		},
		{
			name:       "zero input but cache write",
			meta:       `{"metadata":{"usage":{"inputTokens":0,"outputTokens":5,"cacheWriteInputTokens":9}}}`,
			want:       false,
			wantPrompt: 9,
			wantCompl:  5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{includeUsage: true, responseID: "chatcmpl-1", modelName: "mm"}
			if a.ZeroInputUsageReported() {
				t.Fatal("flag set before any metadata")
			}
			lines, err := bsFeed(a, bsStart, bsText(0, "x"), bsStop, tc.meta)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.ZeroInputUsageReported(); got != tc.want {
				t.Errorf("ZeroInputUsageReported() = %v, want %v", got, tc.want)
			}
			chunks := bsChunks(t, lines)
			last := chunks[len(chunks)-1]
			u, ok := last["usage"].(map[string]any)
			if !ok {
				t.Fatalf("last chunk has no usage: %v", last)
			}
			if u["prompt_tokens"] != tc.wantPrompt || u["completion_tokens"] != tc.wantCompl {
				t.Errorf("usage chunk = %v, want prompt %v completion %v", u, tc.wantPrompt, tc.wantCompl)
			}
			if lines[len(lines)-2] != "data: [DONE]" {
				t.Errorf("[DONE] not emitted after usage chunk, tail %q", lines[len(lines)-2:])
			}
		})
	}
}

func TestBedrockAdapter_StreamZeroInputUsageMetadataBeforeStop(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{includeUsage: true, responseID: "chatcmpl-1", modelName: "mm"}
	lines, err := bsFeed(a, bsStart, bsText(0, "x"),
		`{"metadata":{"usage":{"inputTokens":0,"outputTokens":0}}}`, bsStop)
	if err != nil {
		t.Fatal(err)
	}
	if !a.ZeroInputUsageReported() {
		t.Error("ZeroInputUsageReported() = false, want true")
	}
	if lines[len(lines)-2] != "data: [DONE]" {
		t.Errorf("[DONE] missing, tail %q", lines[len(lines)-2:])
	}
}

func TestBedrockAdapter_TransformResponseZeroInputUsageReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{
			name: "zero input no cache",
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":0,"outputTokens":2,"totalTokens":2}}`,
			want: true,
		},
		{
			name: "usage block absent",
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn"}`,
			want: true,
		},
		{
			name: "normal usage",
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`,
			want: false,
		},
		{
			name: "zero input with cache read",
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":0,"outputTokens":2,"cacheReadInputTokens":12}}`,
			want: false,
		},
		{
			name: "zero input with cache write",
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":0,"outputTokens":2,"cacheWriteInputTokens":3}}`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{modelName: "m", responseID: "chatcmpl-1"}
			if _, err := a.TransformResponse([]byte(tc.in)); err != nil {
				t.Fatal(err)
			}
			if got := a.ZeroInputUsageReported(); got != tc.want {
				t.Errorf("ZeroInputUsageReported() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetAdapter_UsageAnomalyReporter(t *testing.T) {
	t.Parallel()
	if _, ok := GetAdapter("bedrock").(usageAnomalyReporter); !ok {
		t.Error("bedrock adapter does not implement usageAnomalyReporter")
	}
	for _, p := range []string{"openai", "anthropic", "vllm", "azure"} {
		if _, ok := GetAdapter(p).(usageAnomalyReporter); ok {
			t.Errorf("%s adapter unexpectedly implements usageAnomalyReporter", p)
		}
	}
}

// zuWarnRecords returns all captured log records carrying the zero-input msg.
func zuWarnRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		if rec["msg"] == zuWarnMsg {
			out = append(out, rec)
		}
	}
	return out
}

const zuPrompt = "ZU-PROMPT-MARKER-4711"

func TestBedrockHandler_ZeroInputUsageWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		stream     bool
		upstream   func(w http.ResponseWriter, r *http.Request, _ []byte)
		wantWarn   bool
		wantCompl  int
		wantPrompt int
		wantBody   []string
	}{
		{
			name:   "stream zero input",
			stream: true,
			upstream: func(w http.ResponseWriter, r *http.Request, _ []byte) {
				bhWriteFrames(w, bhFrameStart(), bhFrameText("ok"), bhFrameStop("end_turn"), bhFrameMeta(0, 3, 0, 0))
			},
			wantWarn:  true,
			wantCompl: 3,
			wantBody:  []string{`"prompt_tokens":0`, `"completion_tokens":3`, `"finish_reason":"stop"`, "data: [DONE]"},
		},
		{
			name:   "stream normal",
			stream: true,
			upstream: func(w http.ResponseWriter, r *http.Request, _ []byte) {
				bhWriteFrames(w, bhFrameStart(), bhFrameText("ok"), bhFrameStop("end_turn"), bhFrameMeta(10, 3, 0, 0))
			},
			wantWarn:   false,
			wantCompl:  3,
			wantPrompt: 10,
			wantBody:   []string{`"prompt_tokens":10`, "data: [DONE]"},
		},
		{
			name:   "stream zero input with cache read",
			stream: true,
			upstream: func(w http.ResponseWriter, r *http.Request, _ []byte) {
				bhWriteFrames(w, bhFrameStart(), bhFrameText("ok"), bhFrameStop("end_turn"), bhFrameMeta(0, 3, 25, 0))
			},
			wantWarn:   false,
			wantCompl:  3,
			wantPrompt: 25,
			wantBody:   []string{`"prompt_tokens":25`, "data: [DONE]"},
		},
		{
			name: "buffered zero input",
			upstream: func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"pong"}]}},"stopReason":"end_turn","usage":{"inputTokens":0,"outputTokens":3,"totalTokens":3}}`)
			},
			wantWarn:  true,
			wantCompl: 3,
			wantBody:  []string{`"prompt_tokens":0`, `"completion_tokens":3`, `"content":"pong"`},
		},
		{
			name: "buffered normal",
			upstream: func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, bhOKJSON)
			},
			wantWarn:   false,
			wantCompl:  2,
			wantPrompt: 4,
			wantBody:   []string{`"prompt_tokens":4`, `"content":"pong"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, tc.upstream)
			env := newBHEnv(t, bhRegistry(t, up.URL))

			stream := ""
			if tc.stream {
				stream = `"stream":true,"stream_options":{"include_usage":true},`
			}
			resp := env.post(t, `{"model":"claude",`+stream+`"messages":[{"role":"user","content":"`+zuPrompt+`"}]}`)
			if resp.Status != http.StatusOK {
				t.Fatalf("status = %d; body %s", resp.Status, resp.Body)
			}
			for _, w := range tc.wantBody {
				if !strings.Contains(resp.Body, w) {
					t.Errorf("client body lacks %q:\n%s", w, resp.Body)
				}
			}

			row := env.waitUsage(t, 1)
			if row.Status != 200 || row.Prompt != tc.wantPrompt || row.Completion != tc.wantCompl {
				t.Errorf("usage row = %+v, want prompt %d completion %d", row, tc.wantPrompt, tc.wantCompl)
			}

			logs := env.logs.String()
			if strings.Contains(logs, zuPrompt) {
				t.Errorf("prompt text leaked into logs:\n%s", logs)
			}
			recs := zuWarnRecords(t, logs)
			if !tc.wantWarn {
				if len(recs) != 0 {
					t.Fatalf("unexpected warn records: %v", recs)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("warn records = %d, want 1:\n%s", len(recs), logs)
			}
			rec := recs[0]
			if rec["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", rec["level"])
			}
			if rec["model"] != "claude" || rec["provider"] != "bedrock" {
				t.Errorf("model/provider = %v/%v", rec["model"], rec["provider"])
			}
			// The request-ID middleware is not installed by the test server, so
			// only the presence of the attribute can be asserted here.
			if _, ok := rec["request_id"]; !ok {
				t.Errorf("request_id attr missing: %v", rec)
			}
			if rec["completion_tokens"] != float64(tc.wantCompl) {
				t.Errorf("completion_tokens = %v, want %d", rec["completion_tokens"], tc.wantCompl)
			}
			if rec["stream"] != tc.stream {
				t.Errorf("stream = %v, want %v", rec["stream"], tc.stream)
			}
		})
	}
}

func TestBedrockHandler_ZeroInputUsageWarn_NonBedrockAdapter(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":2,"total_tokens":2}}`+"\n\n"+"data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":2,"total_tokens":2}}`)
			})
			reg, err := NewRegistry(zuOpenAIModel(up.URL))
			if err != nil {
				t.Fatal(err)
			}
			env := newBHEnv(t, reg)
			s := ""
			if stream {
				s = `"stream":true,`
			}
			resp := env.post(t, `{"model":"plain",`+s+`"messages":[{"role":"user","content":"`+zuPrompt+`"}]}`)
			if resp.Status != http.StatusOK {
				t.Fatalf("status = %d; body %s", resp.Status, resp.Body)
			}
			row := env.waitUsage(t, 1)
			if row.Prompt != 0 || row.Completion != 2 {
				t.Errorf("usage row = %+v", row)
			}
			logs := env.logs.String()
			if recs := zuWarnRecords(t, logs); len(recs) != 0 {
				t.Errorf("unexpected warn records for non-bedrock adapter: %v", recs)
			}
			if strings.Contains(logs, zuPrompt) {
				t.Errorf("prompt text leaked into logs:\n%s", logs)
			}
		})
	}
}

func zuOpenAIModel(baseURL string) []config.ModelConfig {
	return []config.ModelConfig{{Name: "plain", Provider: "openai", BaseURL: baseURL, APIKey: "k"}}
}
