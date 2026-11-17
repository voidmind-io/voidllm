package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

func TestBedrockAdapter_TransformRequest(t *testing.T) {
	t.Parallel()

	body := `{"model":"m","stream":true,"stream_options":{"include_usage":true},
	"max_tokens":50,"temperature":2,"stop":"x","user":"u","n":3,
	"messages":[
	 {"role":"system","content":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
	 {"role":"user","content":"hi"},
	 {"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
	 {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"k\":1}"}}]},
	 {"role":"tool","tool_call_id":"call_1","content":"res"}],
	"tools":[{"type":"function","function":{"name":"f"}}],
	"tool_choice":"required"}`

	a := &BedrockAdapter{}
	out, err := a.TransformRequest([]byte(body), Model{})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}

	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range doc {
		switch k {
		case "messages", "system", "inferenceConfig", "toolConfig":
		default:
			t.Errorf("unexpected top-level key %q", k)
		}
	}
	s := string(out)
	for _, want := range []string{
		`"system":[{"text":"sys"},{"cachePoint":{"type":"default","ttl":"1h"}}]`,
		`"maxTokens":50`,
		`"temperature":1`,
		`"stopSequences":["x"]`,
		`"toolUse":{"toolUseId":"call_1","name":"f","input":{"k":1}}`,
		`"toolResult":{"toolUseId":"call_1","content":[{"text":"res"}]}`,
		`"toolChoice":{"any":{}}`,
		`{"text":"a"},{"text":"b"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %s\n%s", want, s)
		}
	}
	// Consecutive user messages are merged: user, user(merged), assistant, user(tool).
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := jsonx.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Errorf("messages = %d, want 3 (merged user, assistant, user)", len(req.Messages))
	}
	if !a.streaming || !a.includeUsage {
		t.Errorf("streaming=%v includeUsage=%v, want both true", a.streaming, a.includeUsage)
	}
}

func TestBedrockAdapter_TransformRequestRejects(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, body string }{
		{"no messages", `{"model":"m"}`},
		{"top-level system", `{"model":"m","system":"x","messages":[{"role":"user","content":"a"}]}`},
		{"bad role", `{"model":"m","messages":[{"role":"bogus","content":"a"}]}`},
		{"image part", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url"}]}]}`},
		{"empty user", `{"model":"m","messages":[{"role":"user","content":""}]}`},
		{"bad tool id", `{"model":"m","messages":[{"role":"tool","tool_call_id":"a b","content":"x"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := (&BedrockAdapter{}).TransformRequest([]byte(tc.body), Model{})
			if err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestBedrockAdapter_TransformURLAndHeaders(t *testing.T) {
	t.Parallel()

	m := Model{BedrockModelID: "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"}
	a := &BedrockAdapter{}
	got := a.TransformURL("https://bedrock-runtime.eu-central-1.amazonaws.com/", "chat/completions", m)
	want := "https://bedrock-runtime.eu-central-1.amazonaws.com/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
	a.streaming = true
	got = a.TransformURL("https://h", "", m)
	if !strings.HasSuffix(got, "/converse-stream") {
		t.Errorf("stream URL = %q", got)
	}

	req, err := http.NewRequest(http.MethodPost, got, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.EscapedPath() != "/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse-stream" {
		t.Errorf("escaped path = %q", req.URL.EscapedPath())
	}
	req.Header.Set("Authorization", "Bearer k")
	a.SetHeaders(req, m)
	if req.Header.Get("Accept") != "application/vnd.amazon.eventstream" || req.Header.Get("Authorization") != "Bearer k" {
		t.Errorf("headers = %v", req.Header)
	}
}

func TestBedrockAdapter_TransformResponse(t *testing.T) {
	t.Parallel()

	a := &BedrockAdapter{modelName: "m"}
	in := `{"output":{"message":{"role":"assistant","content":[
	 {"reasoningContent":{"x":1}},{"text":"hel"},{"text":"lo"},
	 {"toolUse":{"toolUseId":"t1","name":"f","input":{"a":1}}}]}},
	 "stopReason":"tool_use","usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":3,"cacheWriteInputTokens":2}}`
	out, err := a.TransformResponse([]byte(in))
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"content":"hello"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":15`,
		`"total_tokens":20`, `"cached_tokens":3`, `"cache_creation_tokens":2`, `"model":"m"`,
		`"arguments":"{\"a\":1}"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("response missing %s\n%s", want, s)
		}
	}

	bad := `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"PII_ab_0123456789abcdef01234567","name":"f","input":{}}}]}}}`
	if _, err := a.TransformResponse([]byte(bad)); err == nil {
		t.Error("pseudonym-shaped tool id must be rejected")
	}
}

func TestBedrockAdapter_StreamBothOrders(t *testing.T) {
	t.Parallel()

	events := map[string]string{
		"start":  `data: {"messageStart":{"role":"assistant"}}`,
		"text":   `data: {"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"hi"}}}`,
		"tstart": `data: {"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"t1","name":"f"}}}}`,
		"tdelta": `data: {"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{}"}}}}`,
		"stop":   `data: {"messageStop":{"stopReason":"end_turn"}}`,
		"meta":   `data: {"metadata":{"usage":{"inputTokens":4,"outputTokens":2,"cacheReadInputTokens":1}}}`,
	}
	for _, order := range [][]string{
		{"start", "text", "tstart", "tdelta", "stop", "meta"},
		{"start", "text", "tstart", "tdelta", "meta", "stop"},
	} {
		a := &BedrockAdapter{includeUsage: true, responseID: "chatcmpl-1"}
		var lines []string
		for _, k := range order {
			out, err := a.TransformStreamLine([]byte(events[k]))
			if err != nil {
				t.Fatalf("%v: %v", order, err)
			}
			for _, l := range out {
				lines = append(lines, string(l))
			}
			if blank, err := a.TransformStreamLine(nil); err != nil || len(blank) != 1 {
				t.Fatalf("blank line: %v %v", blank, err)
			}
		}
		if last := lines[len(lines)-1]; last != "data: [DONE]" {
			t.Errorf("%v: last line = %q, want [DONE]", order, last)
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, `"finish_reason":"stop"`) || !strings.Contains(joined, `"prompt_tokens":5`) {
			t.Errorf("%v: missing finish or usage:\n%s", order, joined)
		}
		if !strings.Contains(joined, `"id":"t1"`) || !strings.Contains(joined, `"arguments":"{}"`) {
			t.Errorf("%v: missing tool call deltas:\n%s", order, joined)
		}
		if u := a.StreamUsage(); u.PromptTokens != 5 || u.TotalTokens != 7 || u.CachedReadTokens != 1 {
			t.Errorf("%v: StreamUsage = %+v", order, u)
		}
	}
}

func TestBedrockAdapter_StreamAborts(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, line string }{
		{"exception", `data: {":exception":"throttlingException"}`},
		{"bad json", `data: {`},
		{"no prefix", `{"messageStart":{}}`},
		{"unknown tool index", `data: {"contentBlockDelta":{"contentBlockIndex":3,"delta":{"toolUse":{"input":"x"}}}}`},
		{"negative index", `data: {"contentBlockStart":{"contentBlockIndex":-1,"start":{"toolUse":{"toolUseId":"a","name":"f"}}}}`},
		{"bad tool id", `data: {"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"a b","name":"f"}}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := (&BedrockAdapter{}).TransformStreamLine([]byte(tc.line)); err != errStreamTransformAborted {
				t.Errorf("err = %v, want errStreamTransformAborted", err)
			}
		})
	}

	a := &BedrockAdapter{}
	stop := []byte(`data: {"messageStop":{"stopReason":"end_turn"}}`)
	if _, err := a.TransformStreamLine(stop); err != nil {
		t.Fatal(err)
	}
	if _, err := a.TransformStreamLine(stop); err != errStreamTransformAborted {
		t.Errorf("second messageStop err = %v, want abort", err)
	}
}

func TestBedrockAdapter_MapUpstreamError(t *testing.T) {
	t.Parallel()

	a := &BedrockAdapter{}
	h := http.Header{}
	h.Set("X-Amzn-Errortype", "AccessDeniedException:http://internal.amazon.com/")
	code, msg := a.MapUpstreamError(403, h, []byte(`{"message":"User: arn:aws:iam::123456789012:user/x is not authorized"}`))
	if code != "permission_denied" || strings.Contains(msg, "123456789012") {
		t.Errorf("got %q %q", code, msg)
	}

	code, _ = a.MapUpstreamError(429, http.Header{}, []byte(`not json`))
	if code != "rate_limit_exceeded" {
		t.Errorf("429 fallback code = %q", code)
	}
	code, _ = a.MapUpstreamError(401, http.Header{}, nil)
	if code != "invalid_api_key" {
		t.Errorf("401 fallback code = %q", code)
	}
}

func TestIsStreamingResponse_BinaryAdapter(t *testing.T) {
	t.Parallel()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Type", "Application/Vnd.Amazon.EventStream")
	if !isStreamingResponse(resp, &BedrockAdapter{}) {
		t.Error("bedrock adapter must treat eventstream as streaming")
	}
	if isStreamingResponse(resp, &AnthropicAdapter{}) || isStreamingResponse(resp, nil) {
		t.Error("other adapters must not treat eventstream as streaming")
	}
}

func TestBedrockAdapter_MapUpstreamError_ValidationTextNeverForwarded(t *testing.T) {
	t.Parallel()

	a := &BedrockAdapter{}
	const body = `{"__type":"ValidationException","message":"bad arn:aws:bedrock:eu-central-1:123456789012:inference-profile/x for account 123456789012: my secret prompt text"}`
	code, msg := a.MapUpstreamError(400, http.Header{}, []byte(body))
	if code != "invalid_request" || msg != "Bedrock rejected the request as invalid" {
		t.Errorf("got %q %q", code, msg)
	}
	for _, leak := range []string{"arn:aws", "123456789012", "secret prompt"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message leaks %q: %q", leak, msg)
		}
	}
}

func TestBedrockAdapter_StreamLifecycleAborts(t *testing.T) {
	t.Parallel()

	const (
		stop   = `data: {"messageStop":{"stopReason":"end_turn"}}`
		meta   = `data: {"metadata":{"usage":{"inputTokens":1,"outputTokens":1}}}`
		delta  = `data: {"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"x"}}}`
		start0 = `data: {"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"a","name":"f"}}}}`
		stop0  = `data: {"contentBlockStop":{"contentBlockIndex":0}}`
	)
	tests := []struct {
		name  string
		lines []string
	}{
		{"delta after messageStop", []string{stop, delta}},
		{"messageStop delta metadata", []string{stop, delta, meta}},
		{"start after messageStop", []string{stop, start0}},
		{"delta after block stop", []string{delta, stop0, delta}},
		{"start after block stop", []string{stop0, start0}},
		{"missing index on delta", []string{`data: {"contentBlockDelta":{"delta":{"text":"x"}}}`}},
		{"missing index on start", []string{`data: {"contentBlockStart":{"start":{"toolUse":{"toolUseId":"a","name":"f"}}}}`}},
		{"missing index on stop", []string{`data: {"contentBlockStop":{}}`}},
		{"duplicate messageStart", []string{`data: {"messageStart":{"role":"assistant"}}`, `data: {"messageStart":{"role":"assistant"}}`}},
		{"negative usage", []string{`data: {"metadata":{"usage":{"inputTokens":-1}}}`}},
		{"huge usage", []string{`data: {"metadata":{"usage":{"outputTokens":9223372036854775807}}}`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{}
			var err error
			for _, l := range tc.lines {
				_, err = a.TransformStreamLine([]byte(l))
				if err != nil {
					break
				}
			}
			if err != errStreamTransformAborted {
				t.Errorf("err = %v, want errStreamTransformAborted", err)
			}
		})
	}
}

func TestBedrockAdapter_TransformResponseRejectsBadUsage(t *testing.T) {
	t.Parallel()

	for _, usage := range []string{`{"inputTokens":-5}`, `{"cacheReadInputTokens":9223372036854775807}`} {
		in := `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn","usage":` + usage + `}`
		if _, err := (&BedrockAdapter{}).TransformResponse([]byte(in)); err == nil {
			t.Errorf("usage %s must be rejected", usage)
		}
	}
}
