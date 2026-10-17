package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBedrockAdapter_TransformURLMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		baseURL   string
		upstream  string
		modelID   string
		streaming bool
		want      string
		wantRaw   string // expected EscapedPath on the wire
	}{
		{
			name:    "derived base url",
			baseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com", modelID: "anthropic.claude-3-haiku-20240307-v1:0",
			want:    "https://bedrock-runtime.eu-central-1.amazonaws.com/model/anthropic.claude-3-haiku-20240307-v1%3A0/converse",
			wantRaw: "/model/anthropic.claude-3-haiku-20240307-v1%3A0/converse",
		},
		{
			name:    "trailing slash trimmed",
			baseURL: "https://h.example/", modelID: "m", want: "https://h.example/model/m/converse", wantRaw: "/model/m/converse",
		},
		{
			name:    "multiple trailing slashes trimmed",
			baseURL: "https://h.example///", modelID: "m", want: "https://h.example/model/m/converse", wantRaw: "/model/m/converse",
		},
		{
			name:    "override with path prefix",
			baseURL: "https://gw.example/bedrock", modelID: "m", want: "https://gw.example/bedrock/model/m/converse", wantRaw: "/bedrock/model/m/converse",
		},
		{
			name:    "fips endpoint",
			baseURL: "https://bedrock-runtime-fips.us-east-1.amazonaws.com", modelID: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
			want:    "https://bedrock-runtime-fips.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
			wantRaw: "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
		},
		{
			name:    "stream path",
			baseURL: "https://h.example", modelID: "m", streaming: true, want: "https://h.example/model/m/converse-stream", wantRaw: "/model/m/converse-stream",
		},
		{
			name:    "upstream path ignored",
			baseURL: "https://h.example", upstream: "chat/completions", modelID: "m", want: "https://h.example/model/m/converse", wantRaw: "/model/m/converse",
		},
		{
			name:    "upstream models path ignored",
			baseURL: "https://h.example", upstream: "models", modelID: "m", want: "https://h.example/model/m/converse", wantRaw: "/model/m/converse",
		},
		{
			name:    "arn escaped once",
			baseURL: "https://h.example",
			modelID: "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
			want:    "https://h.example/model/arn%3Aaws%3Abedrock%3Aeu-central-1%3A123456789012%3Ainference-profile%2Feu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
			wantRaw: "/model/arn%3Aaws%3Abedrock%3Aeu-central-1%3A123456789012%3Ainference-profile%2Feu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
		},
		{
			name:    "arn stream",
			baseURL: "https://h.example", modelID: "arn:aws:bedrock:us-east-1::foundation-model/x.y-v1:0", streaming: true,
			want:    "https://h.example/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A%3Afoundation-model%2Fx.y-v1%3A0/converse-stream",
			wantRaw: "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A%3Afoundation-model%2Fx.y-v1%3A0/converse-stream",
		},
		{
			name:    "unreserved characters kept",
			baseURL: "https://h.example", modelID: "A-b_c.d~e", want: "https://h.example/model/A-b_c.d~e/converse", wantRaw: "/model/A-b_c.d~e/converse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{streaming: tc.streaming}
			got := a.TransformURL(tc.baseURL, tc.upstream, Model{BedrockModelID: tc.modelID})
			if got != tc.want {
				t.Fatalf("URL = %q, want %q", got, tc.want)
			}
			req, err := http.NewRequest(http.MethodPost, got, nil)
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.EscapedPath() != tc.wantRaw {
				t.Errorf("wire path = %q, want %q (double encoding?)", req.URL.EscapedPath(), tc.wantRaw)
			}
			if strings.Contains(req.URL.EscapedPath(), "%25") {
				t.Errorf("wire path %q contains %%25: double encoded", req.URL.EscapedPath())
			}
			// Decoding the path segment must yield the original model ID.
			parts := strings.Split(strings.TrimPrefix(tc.wantRaw, "/"), "/")
			seg := parts[len(parts)-2]
			if dec, err := url.PathUnescape(seg); err != nil || dec != tc.modelID {
				t.Errorf("decoded segment = %q (err %v), want %q", dec, err, tc.modelID)
			}
		})
	}
}

func TestBedrockAdapter_SetHeadersMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		streaming  bool
		preset     map[string]string
		wantAccept string
		wantAuth   string
	}{
		{"non-stream", false, map[string]string{"Authorization": "Bearer k"}, "application/json", "Bearer k"},
		{"stream", true, map[string]string{"Authorization": "Bearer k"}, "application/vnd.amazon.eventstream", "Bearer k"},
		{"client accept overridden non-stream", false, map[string]string{"Authorization": "Bearer k", "Accept": "text/event-stream"}, "application/json", "Bearer k"},
		{"client accept overridden stream", true, map[string]string{"Authorization": "Bearer k", "Accept": "application/json"}, "application/vnd.amazon.eventstream", "Bearer k"},
		{"no key no authorization", false, nil, "application/json", ""},
		{"client content-type overridden", false, map[string]string{"Content-Type": "text/plain"}, "application/json", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodPost, "https://h.example/model/m/converse", nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.preset {
				req.Header.Set(k, v)
			}
			(&BedrockAdapter{streaming: tc.streaming}).SetHeaders(req, Model{})
			if got := req.Header.Get("Accept"); got != tc.wantAccept {
				t.Errorf("Accept = %q, want %q", got, tc.wantAccept)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := req.Header.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
		})
	}
}

func TestGetAdapter_Bedrock(t *testing.T) {
	t.Parallel()
	a1, a2 := GetAdapter("bedrock"), GetAdapter("bedrock")
	if _, ok := a1.(*BedrockAdapter); !ok {
		t.Fatalf("GetAdapter(bedrock) = %T", a1)
	}
	if a1 == a2 {
		t.Error("GetAdapter must return a fresh instance per call")
	}
	if _, ok := a1.(binaryStreamAdapter); !ok {
		t.Error("BedrockAdapter must implement binaryStreamAdapter")
	}
	if _, ok := a1.(upstreamErrorMapper); !ok {
		t.Error("BedrockAdapter must implement upstreamErrorMapper")
	}
	if ct := a1.(binaryStreamAdapter).StreamContentType(); ct != "application/vnd.amazon.eventstream" {
		t.Errorf("StreamContentType = %q", ct)
	}
}

func TestMapBedrockStopReason(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"guardrail_intervened":          "content_filter",
		"content_filtered":              "content_filter",
		"":                              "stop",
		"something_new":                 "stop",
		"END_TURN":                      "stop",
	}
	for in, want := range tests {
		if got := mapBedrockStopReason(in); got != want {
			t.Errorf("mapBedrockStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBedrockAdapter_TransformResponseMatrix(t *testing.T) {
	t.Parallel()

	type check func(t *testing.T, resp map[string]any)
	msg := func(resp map[string]any) map[string]any {
		return resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	}
	finish := func(resp map[string]any) string {
		return resp["choices"].([]any)[0].(map[string]any)["finish_reason"].(string)
	}
	usage := func(resp map[string]any) map[string]any { return resp["usage"].(map[string]any) }

	tests := []struct {
		name  string
		a     *BedrockAdapter
		in    string
		check check
	}{
		{
			name: "text only",
			a:    &BedrockAdapter{modelName: "my-model", responseID: "chatcmpl-77"},
			in:   `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`,
			check: func(t *testing.T, r map[string]any) {
				if msg(r)["content"] != "hello" || msg(r)["role"] != "assistant" {
					t.Errorf("message = %v", msg(r))
				}
				if r["id"] != "chatcmpl-77" || r["model"] != "my-model" || r["object"] != "chat.completion" {
					t.Errorf("envelope = %v", r)
				}
				if finish(r) != "stop" {
					t.Errorf("finish = %q", finish(r))
				}
				u := usage(r)
				if u["prompt_tokens"] != float64(3) || u["completion_tokens"] != float64(2) || u["total_tokens"] != float64(5) {
					t.Errorf("usage = %v", u)
				}
				if _, ok := u["prompt_tokens_details"]; ok {
					t.Errorf("prompt_tokens_details must be absent without cache tokens: %v", u)
				}
				if _, ok := msg(r)["tool_calls"]; ok {
					t.Error("tool_calls must be absent")
				}
			},
		},
		{
			name: "multiple text blocks joined with empty separator",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"a"},{"text":"b"},{"text":"c"}]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if msg(r)["content"] != "abc" {
					t.Errorf("content = %v", msg(r)["content"])
				}
			},
		},
		{
			name: "tool calls only gives null content",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"t1","name":"f","input":{"a":1}}},{"toolUse":{"toolUseId":"t2","name":"g","input":{}}}]}},"stopReason":"tool_use"}`,
			check: func(t *testing.T, r map[string]any) {
				m := msg(r)
				if v, ok := m["content"]; !ok || v != nil {
					t.Errorf("content = %v (present %v), want null", v, ok)
				}
				tcs := m["tool_calls"].([]any)
				if len(tcs) != 2 {
					t.Fatalf("tool_calls = %v", tcs)
				}
				first := tcs[0].(map[string]any)
				fn := first["function"].(map[string]any)
				if first["id"] != "t1" || first["type"] != "function" || fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
					t.Errorf("first tool call = %v", first)
				}
				if tcs[1].(map[string]any)["function"].(map[string]any)["arguments"] != `{}` {
					t.Errorf("second tool call = %v", tcs[1])
				}
				if finish(r) != "tool_calls" {
					t.Errorf("finish = %q", finish(r))
				}
			},
		},
		{
			name: "text and tool call",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"calling"},{"toolUse":{"toolUseId":"t1","name":"f","input":{}}}]}},"stopReason":"tool_use"}`,
			check: func(t *testing.T, r map[string]any) {
				if msg(r)["content"] != "calling" || len(msg(r)["tool_calls"].([]any)) != 1 {
					t.Errorf("message = %v", msg(r))
				}
			},
		},
		{
			name: "tool use with missing input becomes empty object",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"t1","name":"f"}}]}},"stopReason":"tool_use"}`,
			check: func(t *testing.T, r map[string]any) {
				if msg(r)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != "{}" {
					t.Errorf("message = %v", msg(r))
				}
			},
		},
		{
			name: "reasoning and unknown blocks dropped",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"reasoningContent":{"reasoningText":{"text":"SECRET-THINKING"}}},{"citationsContent":{}},{"image":{}},{"text":"answer"}]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if msg(r)["content"] != "answer" {
					t.Errorf("content = %v", msg(r)["content"])
				}
				b, _ := json.Marshal(r)
				if strings.Contains(string(b), "SECRET-THINKING") {
					t.Error("reasoning text leaked")
				}
			},
		},
		{
			name: "only reasoning gives empty content",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"reasoningContent":{}}]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if v, ok := msg(r)["content"]; !ok || v != "" {
					t.Errorf("content = %v (present %v), want empty string", v, ok)
				}
			},
		},
		{
			name: "no content blocks gives empty content",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if v, ok := msg(r)["content"]; !ok || v != "" {
					t.Errorf("content = %v", v)
				}
			},
		},
		{
			name: "cache tokens summed into prompt",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":20}}`,
			check: func(t *testing.T, r map[string]any) {
				u := usage(r)
				if u["prompt_tokens"] != float64(130) || u["completion_tokens"] != float64(5) || u["total_tokens"] != float64(135) {
					t.Errorf("usage = %v", u)
				}
				d := u["prompt_tokens_details"].(map[string]any)
				if d["cached_tokens"] != float64(100) || d["cache_creation_tokens"] != float64(20) {
					t.Errorf("details = %v", d)
				}
			},
		},
		{
			name: "cache read only",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"cacheReadInputTokens":9}}`,
			check: func(t *testing.T, r map[string]any) {
				d := usage(r)["prompt_tokens_details"].(map[string]any)
				if d["cached_tokens"] != float64(9) {
					t.Errorf("details = %v", d)
				}
				if v, ok := d["cache_creation_tokens"]; ok && v != float64(0) {
					t.Errorf("cache_creation_tokens = %v", v)
				}
				if usage(r)["prompt_tokens"] != float64(10) {
					t.Errorf("prompt = %v", usage(r)["prompt_tokens"])
				}
			},
		},
		{
			name: "missing usage is zero",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if usage(r)["total_tokens"] != float64(0) {
					t.Errorf("usage = %v", usage(r))
				}
			},
		},
		{
			name: "synthesized id and model",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn"}`,
			check: func(t *testing.T, r map[string]any) {
				if !strings.HasPrefix(r["id"].(string), "chatcmpl-") || r["model"] != "bedrock" {
					t.Errorf("envelope = %v", r)
				}
			},
		},
		{
			name: "unknown top-level fields ignored",
			a:    &BedrockAdapter{},
			in:   `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"end_turn","metrics":{"latencyMs":3},"trace":{"x":1},"additionalModelResponseFields":{"y":1}}`,
			check: func(t *testing.T, r map[string]any) {
				for _, k := range []string{"metrics", "trace", "additionalModelResponseFields"} {
					if _, ok := r[k]; ok {
						t.Errorf("%q leaked into response", k)
					}
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := tc.a.TransformResponse([]byte(tc.in))
			if err != nil {
				t.Fatalf("TransformResponse: %v", err)
			}
			var r map[string]any
			if err := json.Unmarshal(out, &r); err != nil {
				t.Fatalf("response is not JSON: %v\n%s", err, out)
			}
			tc.check(t, r)
		})
	}
}

func TestBedrockAdapter_TransformResponseStopReasons(t *testing.T) {
	t.Parallel()
	for reason, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
		"model_context_window_exceeded": "length", "tool_use": "tool_calls",
		"guardrail_intervened": "content_filter", "content_filtered": "content_filter", "": "stop", "new": "stop",
	} {
		in := fmt.Sprintf(`{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":%q}`, reason)
		out, err := (&BedrockAdapter{}).TransformResponse([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		if !strings.Contains(string(out), `"finish_reason":"`+want+`"`) {
			t.Errorf("%q: want finish_reason %q in %s", reason, want, out)
		}
	}
}

func TestBedrockAdapter_TransformResponseRejects(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in string }{
		{"empty body", ``},
		{"not json", `not json`},
		{"truncated json", `{"output":`},
		{"array body", `[]`},
		{"tool id with space", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a b","name":"f","input":{}}}]}}}`},
		{"tool id empty", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"","name":"f","input":{}}}]}}}`},
		{"tool id too long", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"` + strings.Repeat("a", 65) + `","name":"f","input":{}}}]}}}`},
		{"tool name with dot", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f.g","input":{}}}]}}}`},
		{"tool name empty", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"","input":{}}}]}}}`},
		{"pseudonym tool id", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"PII_EM_0123456789abcdef01234567","name":"f","input":{}}}]}}}`},
		{"pseudonym tool name", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"PII_EM_0123456789abcdef01234567","input":{}}}]}}}`},
		{"input array", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":[1]}}]}}}`},
		{"input string", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":"x"}}]}}}`},
		{"input number", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":5}}]}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := (&BedrockAdapter{}).TransformResponse([]byte(tc.in))
			if err == nil {
				t.Fatalf("want error, got %s", out)
			}
			if strings.Contains(err.Error(), "PII_") || strings.Contains(err.Error(), "0123456789abcdef") {
				t.Errorf("error echoes upstream content: %q", err.Error())
			}
		})
	}
}

func TestBedrockAdapter_MapUpstreamErrorMatrix(t *testing.T) {
	t.Parallel()

	const (
		secretAcct = "arn:aws:iam::123456789012:user/alice"
	)
	hdr := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("X-Amzn-Errortype", v)
		}
		return h
	}
	body := func(typ, m string) []byte {
		return []byte(fmt.Sprintf(`{"__type":%q,"message":%q}`, typ, m))
	}

	tests := []struct {
		name     string
		status   int
		header   http.Header
		body     []byte
		wantCode string
		wantMsg  string // exact; "" skips
	}{
		{"validation via header", 400, hdr("ValidationException"), []byte(`{"message":"max_tokens too big"}`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation via header with url suffix", 400, hdr("ValidationException:http://internal.amazon.com/"), []byte(`{"message":"bad"}`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation via __type", 400, hdr(""), body("com.amazon.coral.validate#ValidationException", "nope"), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation via code field", 400, hdr(""), []byte(`{"code":"ValidationException","message":"viacode"}`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation capital Message", 400, hdr("ValidationException"), []byte(`{"Message":"capital"}`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation fallback message", 400, hdr("ValidationException"), []byte(`{}`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation non-json body", 400, hdr("ValidationException"), []byte(`<html>`), "invalid_request", "Bedrock rejected the request as invalid"},
		{"header wins over body", 400, hdr("ThrottlingException"), body("ValidationException", "x"), "rate_limit_exceeded", "Bedrock rate limit exceeded"},
		{"access denied hides account", 403, hdr("AccessDeniedException"), []byte(`{"message":"User: ` + secretAcct + ` is not authorized to perform bedrock:InvokeModel"}`), "permission_denied", "Bedrock API key is not authorized for this model"},
		{"unrecognized client", 403, hdr("UnrecognizedClientException"), []byte(`{"message":"The security token included in the request is invalid ` + secretAcct + `"}`), "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"invalid signature", 403, hdr("InvalidSignatureException"), []byte(`{"message":"x"}`), "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"expired token", 400, hdr("ExpiredTokenException"), []byte(`{"message":"expired ` + secretAcct + `"}`), "expired_api_key", "Bedrock API key has expired"},
		{"resource not found", 404, hdr("ResourceNotFoundException"), []byte(`{"message":"model arn:aws:bedrock:us-east-1:123456789012:x"}`), "model_not_found", "Bedrock model or inference profile not found"},
		{"throttling", 429, hdr("ThrottlingException"), []byte(`{"message":"slow down"}`), "rate_limit_exceeded", "Bedrock rate limit exceeded"},
		{"service quota", 400, hdr("ServiceQuotaExceededException"), []byte(`{"message":"quota"}`), "quota_exceeded", "Bedrock service quota exceeded"},
		{"model not ready", 429, hdr("ModelNotReadyException"), []byte(`{"message":"warming"}`), "model_not_ready", "Bedrock model is not ready"},
		{"model timeout", 408, hdr("ModelTimeoutException"), []byte(`{"message":"t"}`), "upstream_timeout", "Bedrock model timed out"},
		{"model error", 424, hdr("ModelErrorException"), []byte(`{"message":"e"}`), "upstream_model_error", "Bedrock model returned an error"},
		{"internal server", 500, hdr("InternalServerException"), []byte(`{"message":"i"}`), "upstream_error", "Bedrock internal error"},
		{"service unavailable", 503, hdr("ServiceUnavailableException"), []byte(`{"message":"s"}`), "upstream_unavailable", "Bedrock service unavailable"},
		{"real invalid bearer key", 403, hdr("AccessDeniedException:http://internal.amazon.com/coral/com.amazon.coral.service/"), []byte(`{"Message":"Authentication failed: Please make sure your API Key is valid."}`), "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"real no model access", 400, hdr("ValidationException:http://internal.amazon.com/coral/com.amazon.bedrock/"), []byte(`{"message":"Error 002: Access to Bedrock models is not allowed for this account"}`), "permission_denied", "Bedrock model access is not enabled for this AWS account"},
		{"access denied other message stays permission", 403, hdr("AccessDeniedException"), []byte(`{"Message":"other"}`), "permission_denied", "Bedrock API key is not authorized for this model"},
		{"validation substring under wrong type ignored", 403, hdr("AccessDeniedException"), []byte(`{"message":"Access to Bedrock models is not allowed"}`), "permission_denied", "Bedrock API key is not authorized for this model"},
		{"unknown name 400", 400, hdr("NovelException"), []byte(`{"message":"leak ` + secretAcct + `"}`), "invalid_request", "Bedrock rejected the request"},
		{"unknown name 401", 401, hdr("NovelException"), nil, "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"unknown name 403", 403, hdr(""), nil, "permission_denied", "Bedrock API key is not authorized for this model"},
		{"unknown name 404", 404, hdr(""), nil, "not_found", "Bedrock resource not found"},
		{"unknown name 429", 429, hdr(""), []byte(`not json`), "rate_limit_exceeded", "Bedrock rate limit exceeded"},
		{"unknown name 500", 500, hdr(""), nil, "upstream_error", "Bedrock returned an error"},
		{"unknown name 502", 502, hdr(""), nil, "upstream_error", "Bedrock returned an error"},
		{"401 with empty header and body", 401, hdr(""), nil, "invalid_api_key", ""},
		{"invalid header chars ignored", 400, hdr("Validation Exception!"), []byte(`{"message":"m"}`), "invalid_request", "Bedrock rejected the request"},
		{"invalid header falls back to body type", 429, hdr("a1b2"), body("ThrottlingException", "m"), "rate_limit_exceeded", "Bedrock rate limit exceeded"},
		{"header 65 chars rejected", 400, hdr(strings.Repeat("A", 65)), nil, "invalid_request", "Bedrock rejected the request"},
		{"__type non string ignored", 400, hdr(""), []byte(`{"__type":5,"message":"m"}`), "invalid_request", "Bedrock rejected the request"},
		{"__type with injection chars", 400, hdr(""), body("x#Validation\"Exception", "m"), "invalid_request", "Bedrock rejected the request"},
		{"empty body empty header 418", 418, hdr(""), nil, "upstream_error", "Bedrock returned an error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, msg := (&BedrockAdapter{}).MapUpstreamError(tc.status, tc.header, tc.body)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.wantMsg != "" && msg != tc.wantMsg {
				t.Errorf("message = %q, want %q", msg, tc.wantMsg)
			}
			for _, leak := range []string{"123456789012", "alice", "slow down", "warming", "max_tokens", "nope", "viacode", "capital", "Authentication failed", "Error 002", "this account"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message %q leaks %q", msg, leak)
				}
			}
		})
	}
}
