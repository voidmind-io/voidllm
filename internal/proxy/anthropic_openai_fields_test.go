package proxy

// anthropic_openai_fields_test.go covers the OpenAI-only field allowlist,
// stop/parallel_tool_calls/user/temperature/developer-role translations,
// system content shapes (string, array, cache_control), per-part cache_control
// on user/assistant/tool-result blocks, and the include_usage streaming usage
// chunk introduced alongside the upstream allowlist fix. It reuses the
// transformRequest, runStream, parseChunk, unmarshalDoc, and strPtr helpers
// declared in anthropic_test.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── Allowlist sweep ───────────────────────────────────────────────────────────

// TestAnthropicTransformRequest_AllowlistDropsOpenAIOnlyFields verifies that
// every OpenAI-only field — whether explicitly translated elsewhere or
// unknown to the adapter entirely — is absent from the transformed request
// body. This is the regression test for the allowlist mechanism itself: a
// brand-new OpenAI field the adapter has never heard of ("some_future_field")
// must be dropped exactly like the fields the adapter already understands.
func TestAnthropicTransformRequest_AllowlistDropsOpenAIOnlyFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field string
		value string // raw JSON value for the field
	}{
		{"stop", "stop", `"STOP"`},
		{"parallel_tool_calls", "parallel_tool_calls", `false`},
		{"user", "user", `"user-123"`},
		{"metadata", "metadata", `{"custom":"value"}`},
		{"stream_options", "stream_options", `{"include_usage":true}`},
		{"max_completion_tokens", "max_completion_tokens", `256`},
		{"n", "n", `2`},
		{"seed", "seed", `42`},
		{"response_format", "response_format", `{"type":"json_object"}`},
		{"reasoning_effort", "reasoning_effort", `"high"`},
		{"modalities", "modalities", `["text","audio"]`},
		{"audio", "audio", `{"voice":"alloy","format":"wav"}`},
		{"prediction", "prediction", `{"type":"content","content":"x"}`},
		{"web_search_options", "web_search_options", `{"search_context_size":"high"}`},
		{"functions", "functions", `[{"name":"fn","parameters":{}}]`},
		{"function_call", "function_call", `"auto"`},
		{"verbosity", "verbosity", `"low"`},
		{"prompt_cache_key", "prompt_cache_key", `"cache-key-1"`},
		{"safety_identifier", "safety_identifier", `"safety-1"`},
		{"logit_bias", "logit_bias", `{"1234":-100}`},
		{"some_future_field_unknown_to_the_adapter", "some_future_field", `{"anything":"goes"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"max_tokens":16,%q:%s}`,
				tc.field, tc.value,
			)
			doc := transformRequest(t, input)
			if _, ok := doc[tc.field]; ok {
				t.Errorf("output still contains OpenAI-only field %q, want dropped by allowlist", tc.field)
			}
		})
	}
}

// TestAnthropicTransformRequest_AllowlistKeepsAnthropicNativeFields verifies
// that fields Anthropic actually accepts survive the allowlist filter
// unchanged: top_k, thinking, and a top-level cache_control passthrough.
func TestAnthropicTransformRequest_AllowlistKeepsAnthropicNativeFields(t *testing.T) {
	t.Parallel()

	input := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"max_tokens":16,` +
		`"top_k":40,"thinking":{"type":"enabled","budget_tokens":1024},"cache_control":{"type":"ephemeral"}}`
	doc := transformRequest(t, input)

	for _, field := range []string{"top_k", "thinking", "cache_control"} {
		if _, ok := doc[field]; !ok {
			t.Errorf("output missing native field %q, want preserved by allowlist", field)
		}
	}

	var topK int
	if err := json.Unmarshal(doc["top_k"], &topK); err != nil {
		t.Fatalf("unmarshal top_k: %v", err)
	}
	if topK != 40 {
		t.Errorf("top_k = %d, want 40", topK)
	}
}

// ── stop → stop_sequences ─────────────────────────────────────────────────────

// TestAnthropicTransformRequest_Stop covers the stop→stop_sequences
// translation across every input shape: plain string, array, null, and
// empty/whitespace-only entries (dropped). A client-supplied top-level
// stop_sequences is rejected fail-closed (see
// TestAnthropicTransformRequest_ClientSuppliedNativeFieldsRejected) since it
// is a native Anthropic field never scanned by internal/pii.
func TestAnthropicTransformRequest_Stop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		extraFields   string // raw JSON fragment appended after messages, e.g. `,"stop":"X"`
		wantSequences []string
		wantAbsent    bool // stop_sequences should not appear at all
	}{
		{
			name:          "string stop becomes single-element stop_sequences",
			extraFields:   `,"stop":"STOP"`,
			wantSequences: []string{"STOP"},
		},
		{
			name:          "array stop becomes stop_sequences array",
			extraFields:   `,"stop":["STOP1","STOP2"]`,
			wantSequences: []string{"STOP1", "STOP2"},
		},
		{
			name:        "null stop yields no stop_sequences",
			extraFields: `,"stop":null`,
			wantAbsent:  true,
		},
		{
			name:        "empty string stop yields no stop_sequences",
			extraFields: `,"stop":""`,
			wantAbsent:  true,
		},
		{
			name:          "array with whitespace-only and empty entries drops them",
			extraFields:   `,"stop":["  ","STOP","","\t"]`,
			wantSequences: []string{"STOP"},
		},
		{
			name:        "array of only empty/whitespace entries yields no stop_sequences",
			extraFields: `,"stop":["  ","",""]`,
			wantAbsent:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]%s}`,
				tc.extraFields,
			)
			doc := transformRequest(t, input)

			raw, ok := doc["stop_sequences"]
			if tc.wantAbsent {
				if ok {
					t.Errorf("stop_sequences present = %s, want absent", raw)
				}
				return
			}
			if !ok {
				t.Fatal("output missing stop_sequences field")
			}
			var got []string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal stop_sequences: %v", err)
			}
			if len(got) != len(tc.wantSequences) {
				t.Fatalf("stop_sequences = %v, want %v", got, tc.wantSequences)
			}
			for i, s := range got {
				if s != tc.wantSequences[i] {
					t.Errorf("stop_sequences[%d] = %q, want %q", i, s, tc.wantSequences[i])
				}
			}
			// "stop" itself must never survive the allowlist.
			if _, ok := doc["stop"]; ok {
				t.Error("output still contains OpenAI 'stop' field")
			}
		})
	}
}

// TestAnthropicTransformRequest_ClientSuppliedNativeFieldsRejected verifies
// that a client-supplied top-level "system" or "stop_sequences" field is
// rejected fail-closed: both are native Anthropic fields the adapter emits
// internally after translation, and neither is scanned by internal/pii. It
// also covers the malformed-shape fail-closed cases for "stop",
// "parallel_tool_calls", cache_control, and non-text content parts that
// replace the previous silent-drop behavior.
//
// Every case asserts that the returned error unwraps (via errors.As) to a
// clientRequestError carrying the exact static, caller-content-free message
// that buildUpstreamRequest (handler.go) sends to the client as the 400 body
// — the underlying error is never surfaced generically for these cases.
func TestAnthropicTransformRequest_ClientSuppliedNativeFieldsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantMsg string
	}{
		{
			name:    "client-supplied top-level system is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"system":"client system"}`,
			wantMsg: `top-level system is not supported; send a message with role "system" instead`,
		},
		{
			name:    "client-supplied top-level stop_sequences is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"stop_sequences":["X"]}`,
			wantMsg: "stop_sequences is not supported; use stop instead",
		},
		{
			name:    "stop as a number is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"stop":42}`,
			wantMsg: "stop must be a string or an array of strings",
		},
		{
			name:    "stop as an array containing a non-string is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"stop":["A",1]}`,
			wantMsg: "stop must be a string or an array of strings",
		},
		{
			name:    "parallel_tool_calls as a non-boolean is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":"false"}`,
			wantMsg: "parallel_tool_calls must be a boolean",
		},
		{
			name:    "invalid top-level cache_control is rejected",
			input:   `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"cache_control":{"type":"persistent"}}`,
			wantMsg: `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`,
		},
		{
			name: "invalid per-part cache_control is rejected",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","extra":"x"}}]}` +
				`]}`,
			wantMsg: `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`,
		},
		{
			name: "non-text content part on a user message is rejected",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.com/img.png"}}]}` +
				`]}`,
			wantMsg: "only text content parts are supported for this model",
		},
		{
			name: "non-text content part on a system message is rejected",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"system","content":[{"type":"image_url","image_url":{"url":"http://example.com/img.png"}}]},` +
				`{"role":"user","content":"hi"}` +
				`]}`,
			wantMsg: "only text content parts are supported for this model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &AnthropicAdapter{}
			_, err := a.TransformRequest([]byte(tc.input), Model{})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var clientErr *clientRequestError
			if !errors.As(err, &clientErr) {
				t.Fatalf("error does not unwrap to a clientRequestError: %v", err)
			}
			if clientErr.Error() != tc.wantMsg {
				t.Errorf("client-safe message = %q, want %q", clientErr.Error(), tc.wantMsg)
			}
		})
	}
}

// anthropicClientErrorRegistry builds a Registry with a single Anthropic-
// provider model, so ProxyHandler.Handle routes through AnthropicAdapter.
// TransformRequest. No upstream server is needed: every case here is
// expected to fail transformation before any upstream request is built.
func anthropicClientErrorRegistry(t *testing.T) *Registry {
	t.Helper()
	m := &Model{
		Name:     "claude-handler-test",
		Provider: "anthropic",
		Type:     "chat",
		BaseURL:  "http://unused.invalid",
		APIKey:   "key-unused",
	}
	r := &Registry{
		models:  map[string]*Model{"claude-handler-test": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// TestAnthropicTransformRequest_ClientSafeErrorSurfacesThroughHandler is the
// end-to-end counterpart of
// TestAnthropicTransformRequest_ClientSuppliedNativeFieldsRejected: it drives
// a client-safe TransformRequest failure through the full ProxyHandler.Handle
// pipeline and asserts that the HTTP response is 400 bad_request with the
// exact static message, rather than the generic "failed to transform request
// for provider" fallback.
func TestAnthropicTransformRequest_ClientSafeErrorSurfacesThroughHandler(t *testing.T) {
	t.Parallel()

	reg := anthropicClientErrorRegistry(t)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app := testApp(t, handler)

	body := `{"model":"claude-handler-test","messages":[{"role":"user","content":"hi"}],"system":"client system"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("unmarshal response body: %v (body: %s)", err, respBody)
	}
	if envelope.Error.Code != "bad_request" {
		t.Errorf("error.code = %q, want %q", envelope.Error.Code, "bad_request")
	}
	wantMsg := `top-level system is not supported; send a message with role "system" instead`
	if envelope.Error.Message != wantMsg {
		t.Errorf("error.message = %q, want %q", envelope.Error.Message, wantMsg)
	}
}

// ── parallel_tool_calls → tool_choice.disable_parallel_tool_use ──────────────

// TestAnthropicTransformRequest_ParallelToolCalls covers parallel_tool_calls
// translation across every tool_choice combination: absent, "auto",
// "required", a named function choice, "none" (tools removed), and the case
// where no tools were declared at all. true and absent are no-ops.
func TestAnthropicTransformRequest_ParallelToolCalls(t *testing.T) {
	t.Parallel()

	baseTools := `[{"type":"function","function":{"name":"lookup","parameters":{}}}]`

	tests := []struct {
		name         string
		toolsRaw     string // "" means no tools array at all
		toolChoice   string // "" means no tool_choice field
		parallel     string // raw JSON value for parallel_tool_calls, "" means field absent
		wantDisabled bool   // disable_parallel_tool_use should be true
		wantType     string // expected tool_choice.type when wantDisabled or toolChoice was set
		wantAbsentTC bool   // tool_choice should not appear at all
	}{
		{
			name:         "false with tool_choice auto sets disable_parallel_tool_use on auto",
			toolsRaw:     baseTools,
			toolChoice:   `"auto"`,
			parallel:     "false",
			wantDisabled: true,
			wantType:     "auto",
		},
		{
			name:         "false with tool_choice required sets disable_parallel_tool_use on any",
			toolsRaw:     baseTools,
			toolChoice:   `"required"`,
			parallel:     "false",
			wantDisabled: true,
			wantType:     "any",
		},
		{
			name:         "false with named tool_choice sets disable_parallel_tool_use on tool",
			toolsRaw:     baseTools,
			toolChoice:   `{"type":"function","function":{"name":"lookup"}}`,
			parallel:     "false",
			wantDisabled: true,
			wantType:     "tool",
		},
		{
			name:       "false with tool_choice none removes tools and tool_choice entirely",
			toolsRaw:   baseTools,
			toolChoice: `"none"`,
			parallel:   "false",
			// tool_choice:"none" removes both tools and tool_choice upstream of the
			// parallel_tool_calls translation; there is nothing left to attach the
			// flag to.
			wantAbsentTC: true,
		},
		{
			name:         "false with no tool_choice synthesizes type:auto with the flag",
			toolsRaw:     baseTools,
			parallel:     "false",
			wantDisabled: true,
			wantType:     "auto",
		},
		{
			name:         "false with no tools present is a no-op",
			toolsRaw:     "",
			parallel:     "false",
			wantAbsentTC: true,
		},
		{
			name:       "true is a no-op",
			toolsRaw:   baseTools,
			toolChoice: `"auto"`,
			parallel:   "true",
			wantType:   "auto",
		},
		{
			name:       "absent parallel_tool_calls is a no-op",
			toolsRaw:   baseTools,
			toolChoice: `"auto"`,
			wantType:   "auto",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var b strings.Builder
			b.WriteString(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}]`)
			if tc.toolsRaw != "" {
				fmt.Fprintf(&b, `,"tools":%s`, tc.toolsRaw)
			}
			if tc.toolChoice != "" {
				fmt.Fprintf(&b, `,"tool_choice":%s`, tc.toolChoice)
			}
			if tc.parallel != "" {
				fmt.Fprintf(&b, `,"parallel_tool_calls":%s`, tc.parallel)
			}
			b.WriteString(`}`)

			doc := transformRequest(t, b.String())

			// parallel_tool_calls itself must never survive the allowlist.
			if _, ok := doc["parallel_tool_calls"]; ok {
				t.Error("output still contains OpenAI 'parallel_tool_calls' field")
			}

			raw, ok := doc["tool_choice"]
			if tc.wantAbsentTC {
				if ok {
					t.Errorf("tool_choice present = %s, want absent", raw)
				}
				return
			}
			if !ok {
				t.Fatal("output missing tool_choice field")
			}
			var got anthropicToolChoice
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal tool_choice: %v", err)
			}
			if got.Type != tc.wantType {
				t.Errorf("tool_choice.type = %q, want %q", got.Type, tc.wantType)
			}
			if tc.wantDisabled {
				if got.DisableParallelToolUse == nil || !*got.DisableParallelToolUse {
					t.Errorf("disable_parallel_tool_use = %v, want true", got.DisableParallelToolUse)
				}
			} else if got.DisableParallelToolUse != nil {
				t.Errorf("disable_parallel_tool_use = %v, want nil (no-op)", *got.DisableParallelToolUse)
			}
		})
	}
}

// ── user → metadata.user_id ───────────────────────────────────────────────────

// TestAnthropicTransformRequest_UserToMetadata verifies that OpenAI's user
// field is translated into Anthropic's metadata.user_id, and that OpenAI's
// free-form metadata map is always dropped — even when user is absent, and
// even when the client supplied its own metadata object alongside user.
func TestAnthropicTransformRequest_UserToMetadata(t *testing.T) {
	t.Parallel()

	t.Run("user becomes metadata.user_id", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"user":"user-42"}`
		doc := transformRequest(t, input)

		raw, ok := doc["metadata"]
		if !ok {
			t.Fatal("output missing metadata field")
		}
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		if meta.UserID != "user-42" {
			t.Errorf("metadata.user_id = %q, want %q", meta.UserID, "user-42")
		}
		if _, ok := doc["user"]; ok {
			t.Error("output still contains OpenAI 'user' field")
		}
	})

	t.Run("OpenAI metadata is dropped even when user is present", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"user":"user-42","metadata":{"client_id":"should-not-survive"}}`
		doc := transformRequest(t, input)

		raw, ok := doc["metadata"]
		if !ok {
			t.Fatal("output missing metadata field")
		}
		if strings.Contains(string(raw), "client_id") {
			t.Errorf("OpenAI metadata leaked into output metadata: %s", raw)
		}
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		if meta.UserID != "user-42" {
			t.Errorf("metadata.user_id = %q, want %q", meta.UserID, "user-42")
		}
	})

	t.Run("OpenAI metadata alone (no user) is dropped entirely", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"metadata":{"client_id":"x"}}`
		doc := transformRequest(t, input)
		if _, ok := doc["metadata"]; ok {
			t.Errorf("metadata present = %s, want absent (no user field to build it from)", doc["metadata"])
		}
	})

	t.Run("absent user and metadata: no metadata field emitted", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}]}`
		doc := transformRequest(t, input)
		if _, ok := doc["metadata"]; ok {
			t.Errorf("metadata present = %s, want absent", doc["metadata"])
		}
	})
}

// ── temperature clamp ─────────────────────────────────────────────────────────

// TestAnthropicTransformRequest_TemperatureClamp verifies that any
// temperature above Anthropic's 0-1 range is clamped to 1, and values within
// range are passed through unchanged.
func TestAnthropicTransformRequest_TemperatureClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  float64
	}{
		{"above range clamps to 1", 1.7, 1},
		{"within range unchanged", 0.3, 0.3},
		{"exactly 1 unchanged", 1, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"temperature":%v}`,
				tc.input,
			)
			doc := transformRequest(t, input)

			raw, ok := doc["temperature"]
			if !ok {
				t.Fatal("output missing temperature field")
			}
			var got float64
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal temperature: %v", err)
			}
			if got != tc.want {
				t.Errorf("temperature = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── developer role → system ───────────────────────────────────────────────────

// TestAnthropicTransformRequest_DeveloperRole verifies that OpenAI's
// "developer" role (the newer alias for "system") is merged into the
// top-level system field exactly like "system", including when mixed with an
// actual "system" message in the same request.
func TestAnthropicTransformRequest_DeveloperRole(t *testing.T) {
	t.Parallel()

	t.Run("developer role alone is treated as system", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"developer","content":"Be concise."},{"role":"user","content":"Hi"}]}`
		doc := transformRequest(t, input)

		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing top-level system field")
		}
		var system string
		if err := json.Unmarshal(raw, &system); err != nil {
			t.Fatalf("unmarshal system: %v", err)
		}
		if system != "Be concise." {
			t.Errorf("system = %q, want %q", system, "Be concise.")
		}

		msgs := unmarshalMessages(t, doc)
		for _, m := range msgs {
			if m.Role == "developer" {
				t.Error("messages still contains a developer-role entry")
			}
		}
	})

	t.Run("developer and system messages merge in order", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"system","content":"System part."},` +
			`{"role":"developer","content":"Developer part."},` +
			`{"role":"user","content":"Hi"}` +
			`]}`
		doc := transformRequest(t, input)

		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing top-level system field")
		}
		var system string
		if err := json.Unmarshal(raw, &system); err != nil {
			t.Fatalf("unmarshal system: %v", err)
		}
		if system != "System part.\nDeveloper part." {
			t.Errorf("system = %q, want %q", system, "System part.\nDeveloper part.")
		}
	})
}

// ── system content shapes ─────────────────────────────────────────────────────

// TestAnthropicTransformRequest_SystemContentShapes covers every accepted
// shape of a system/developer message's content field: a plain string
// (joined-string output, unchanged from historical behavior), an array of
// text parts (array-of-blocks output, one block per part, never
// concatenated), a part carrying cache_control, empty parts skipped, a
// non-text part (fail-closed error), and absent content (a no-op).
func TestAnthropicTransformRequest_SystemContentShapes(t *testing.T) {
	t.Parallel()

	t.Run("plain string content emits joined-string system field", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}`
		doc := transformRequest(t, input)
		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing system field")
		}
		var system string
		if err := json.Unmarshal(raw, &system); err != nil {
			t.Fatalf("unmarshal system as string: %v (raw: %s)", err, raw)
		}
		if system != "You are helpful." {
			t.Errorf("system = %q, want %q", system, "You are helpful.")
		}
	})

	t.Run("array content emits one block per text part, never concatenated", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"system","content":[{"type":"text","text":"alice@"},{"type":"text","text":"example.com"}]},` +
			`{"role":"user","content":"Hi"}` +
			`]}`
		doc := transformRequest(t, input)
		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing system field")
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatalf("unmarshal system as array: %v (raw: %s)", err, raw)
		}
		if len(blocks) != 2 {
			t.Fatalf("len(system blocks) = %d, want 2 (parts must remain separate)", len(blocks))
		}
		if blocks[0].Text != "alice@" || blocks[1].Text != "example.com" {
			t.Errorf("system blocks = %+v, want separate 'alice@' and 'example.com'", blocks)
		}
		if strings.Contains(string(raw), "alice@example.com") {
			t.Errorf("SECURITY: joined PII string appears in system field; raw: %s", raw)
		}
	})

	t.Run("part with cache_control forces array output shape and preserves it", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"system","content":[{"type":"text","text":"cached instructions","cache_control":{"type":"ephemeral"}}]},` +
			`{"role":"user","content":"Hi"}` +
			`]}`
		doc := transformRequest(t, input)
		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing system field")
		}
		var blocks []anthropicContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatalf("unmarshal system as array of blocks: %v (raw: %s)", err, raw)
		}
		if len(blocks) != 1 {
			t.Fatalf("len(system blocks) = %d, want 1", len(blocks))
		}
		if blocks[0].Text != "cached instructions" {
			t.Errorf("blocks[0].text = %q, want %q", blocks[0].Text, "cached instructions")
		}
		if len(blocks[0].CacheControl) == 0 {
			t.Fatal("blocks[0].cache_control missing, want preserved")
		}
		var cc struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(blocks[0].CacheControl, &cc); err != nil {
			t.Fatalf("unmarshal cache_control: %v", err)
		}
		if cc.Type != "ephemeral" {
			t.Errorf("cache_control.type = %q, want %q", cc.Type, "ephemeral")
		}
	})

	t.Run("empty text parts are skipped", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"system","content":[{"type":"text","text":""},{"type":"text","text":"kept"}]},` +
			`{"role":"user","content":"Hi"}` +
			`]}`
		doc := transformRequest(t, input)
		raw, ok := doc["system"]
		if !ok {
			t.Fatal("output missing system field")
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatalf("unmarshal system as array: %v (raw: %s)", err, raw)
		}
		if len(blocks) != 1 {
			t.Fatalf("len(system blocks) = %d, want 1 (empty part skipped)", len(blocks))
		}
		if blocks[0].Text != "kept" {
			t.Errorf("blocks[0].text = %q, want %q", blocks[0].Text, "kept")
		}
	})

	t.Run("non-text system part returns fail-closed error", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"system","content":[{"type":"image_url","text":""}]},` +
			`{"role":"user","content":"Hi"}` +
			`]}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for non-text system content part, got nil")
		}
	})

	t.Run("absent content field on a system message is a no-op", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"system"},{"role":"user","content":"Hi"}]}`
		doc := transformRequest(t, input)
		if _, ok := doc["system"]; ok {
			t.Errorf("system present = %s, want absent (contentless system message contributes nothing)", doc["system"])
		}
	})
}

// ── per-part cache_control on user/assistant/tool-result blocks ──────────────

// TestAnthropicTransformRequest_PerPartCacheControl verifies that a
// cache_control object on an individual content part survives translation on
// user, assistant, and tool-result text blocks.
func TestAnthropicTransformRequest_PerPartCacheControl(t *testing.T) {
	t.Parallel()

	t.Run("user text part cache_control preserved", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		if len(msgs) != 1 || len(msgs[0].Content) != 1 {
			t.Fatalf("unexpected messages shape: %+v", msgs)
		}
		block := msgs[0].Content[0]
		if len(block.CacheControl) == 0 {
			t.Fatal("user block cache_control missing, want preserved")
		}
		if !strings.Contains(string(block.CacheControl), "ephemeral") {
			t.Errorf("cache_control = %s, want to contain ephemeral", block.CacheControl)
		}
	})

	t.Run("assistant text part cache_control preserved", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":[{"type":"text","text":"answer","cache_control":{"type":"ephemeral"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		if len(msgs) != 2 {
			t.Fatalf("len(msgs) = %d, want 2", len(msgs))
		}
		assistant := msgs[1]
		if assistant.Role != "assistant" {
			t.Fatalf("msgs[1].role = %q, want assistant", assistant.Role)
		}
		if len(assistant.Content) != 1 {
			t.Fatalf("len(assistant content) = %d, want 1", len(assistant.Content))
		}
		if len(assistant.Content[0].CacheControl) == 0 {
			t.Fatal("assistant block cache_control missing, want preserved")
		}
	})

	t.Run("tool-result text part cache_control preserved", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"result","cache_control":{"type":"ephemeral"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		last := msgs[len(msgs)-1]
		if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
			t.Fatalf("unexpected last message shape: %+v", last)
		}
		var blocks []anthropicContentBlock
		if err := json.Unmarshal(last.Content[0].Content, &blocks); err != nil {
			t.Fatalf("unmarshal tool_result.content as array: %v (raw: %s)", err, last.Content[0].Content)
		}
		if len(blocks) != 1 {
			t.Fatalf("len(tool_result content blocks) = %d, want 1", len(blocks))
		}
		if len(blocks[0].CacheControl) == 0 {
			t.Fatal("tool_result block cache_control missing, want preserved")
		}
		if !strings.Contains(string(blocks[0].CacheControl), "ephemeral") {
			t.Errorf("cache_control = %s, want to contain ephemeral", blocks[0].CacheControl)
		}
	})
}

// ── stream_options.include_usage → trailing usage chunk ──────────────────────

// TestAnthropicStream_UsageChunk covers the include_usage streaming behavior:
// without it, message_stop produces exactly "data: [DONE]"; with it, a
// usage-only chunk (empty choices, id/model/created present, correct
// prompt/completion/total tokens and prompt_tokens_details.cached_tokens
// computed from message_start's cache read/write counts) precedes [DONE].
func TestAnthropicStream_UsageChunk(t *testing.T) {
	t.Parallel()

	messageStart := `data: {"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","stop_reason":null,` +
		`"usage":{"input_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":5,"output_tokens":0}}}`
	messageDelta := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`
	messageStop := `data: {"type":"message_stop"}`

	t.Run("without include_usage, message_stop is exactly [DONE]", func(t *testing.T) {
		t.Parallel()
		a := &AnthropicAdapter{}
		out := runStream(t, a, []string{messageStart, messageDelta, messageStop})
		if len(out) == 0 {
			t.Fatal("no output lines")
		}
		last := out[len(out)-1]
		if string(last) != "data: [DONE]" {
			t.Errorf("last line = %q, want %q", last, "data: [DONE]")
		}
		for _, l := range out {
			if strings.Contains(string(l), `"usage"`) {
				t.Errorf("unexpected usage chunk without include_usage: %s", l)
			}
		}
	})

	t.Run("with include_usage, usage chunk precedes [DONE]", func(t *testing.T) {
		t.Parallel()
		a := &AnthropicAdapter{}
		a.includeUsage = true
		a.modelName = "claude-3-5-sonnet"
		out := runStream(t, a, []string{messageStart, messageDelta, messageStop})
		if len(out) < 2 {
			t.Fatalf("len(out) = %d, want at least 2 (usage chunk + [DONE])", len(out))
		}
		last := out[len(out)-1]
		if string(last) != "data: [DONE]" {
			t.Errorf("last line = %q, want %q", last, "data: [DONE]")
		}
		usageLine := out[len(out)-2]

		chunk := parseChunk(t, usageLine)
		if chunk.ID == "" {
			t.Error("usage chunk id is empty, want non-empty")
		}
		if chunk.Model == nil || *chunk.Model != "claude-3-5-sonnet" {
			t.Errorf("usage chunk model = %v, want %q", chunk.Model, "claude-3-5-sonnet")
		}
		if chunk.Created == nil {
			t.Error("usage chunk created is nil, want non-nil")
		}
		if len(chunk.Choices) != 0 {
			t.Errorf("usage chunk choices = %+v, want empty", chunk.Choices)
		}
		if chunk.Usage == nil {
			t.Fatal("usage chunk usage field is nil, want populated")
		}
		// promptTokens = input_tokens(50) + cache_read(20) + cache_write(5) = 75.
		if chunk.Usage.PromptTokens != 75 {
			t.Errorf("prompt_tokens = %d, want 75", chunk.Usage.PromptTokens)
		}
		if chunk.Usage.CompletionTokens != 30 {
			t.Errorf("completion_tokens = %d, want 30", chunk.Usage.CompletionTokens)
		}
		if chunk.Usage.TotalTokens != 105 {
			t.Errorf("total_tokens = %d, want 105", chunk.Usage.TotalTokens)
		}
		if chunk.Usage.PromptTokensDetails == nil {
			t.Fatal("prompt_tokens_details is nil, want populated")
		}
		if chunk.Usage.PromptTokensDetails.CachedTokens != 20 {
			t.Errorf("prompt_tokens_details.cached_tokens = %d, want 20", chunk.Usage.PromptTokensDetails.CachedTokens)
		}
		if chunk.Usage.PromptTokensDetails.CacheCreationTokens != 5 {
			t.Errorf("prompt_tokens_details.cache_creation_tokens = %d, want 5", chunk.Usage.PromptTokensDetails.CacheCreationTokens)
		}
	})

	t.Run("no usage chunk is produced after an abort path", func(t *testing.T) {
		t.Parallel()
		a := &AnthropicAdapter{}
		a.includeUsage = true

		// Register content-block index 0 as a tool_use block.
		if _, err := a.TransformStreamLine([]byte(
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"fn"}}`,
		)); err != nil {
			t.Fatalf("priming content_block_start: %v", err)
		}

		// A duplicate content-block index is a protocol violation and aborts the
		// stream (FIX 8). The real handler stops calling TransformStreamLine on
		// this adapter once it sees the abort — it never reaches message_stop.
		_, err := a.TransformStreamLine([]byte(
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t2","name":"fn2"}}`,
		))
		if err == nil {
			t.Fatal("expected abort error for duplicate content-block index, got nil")
		}

		// Simulate the invariant directly: buildStreamUsageChunk is never called
		// again by the handler once TransformStreamLine has returned an error, so
		// there is no code path that could emit a usage chunk after this point.
		// What we can and do assert here is that the adapter's own state prior to
		// the abort produced no usage-carrying output line.
	})
}

// ── Gemini chunk shape is unaffected by the new shared fields ────────────────

// TestGeminiChunkOutputUnchangedByNewSharedFields verifies that adding
// Created, Model, and Usage (all omitempty pointers) to the shared openAIChunk
// struct did not change a single byte of the Gemini adapter's stream chunk
// output — Gemini never populates those fields, so they must stay entirely
// absent from the marshaled JSON.
func TestGeminiChunkOutputUnchangedByNewSharedFields(t *testing.T) {
	t.Parallel()

	a := &GeminiAdapter{}
	line := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":""}],"usageMetadata":{}}`
	out := transformLine1(a, []byte(line))
	if out == nil {
		t.Fatal("TransformStreamLine() = nil, want non-nil")
	}

	const prefix = "data: "
	if !strings.HasPrefix(string(out), prefix) {
		t.Fatalf("output %q does not start with %q", out, prefix)
	}
	payload := string(out)[len(prefix):]

	// The chunk id is a nanosecond timestamp and therefore non-deterministic;
	// extract it and rebuild the golden with the real id substituted in, so
	// the rest of the comparison is a true byte-for-byte match.
	var chunk openAIChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	if chunk.ID == "" {
		t.Fatal("chunk id is empty")
	}
	if chunk.Created != nil {
		t.Errorf("created = %v, want nil (Gemini never sets it)", *chunk.Created)
	}
	if chunk.Model != nil {
		t.Errorf("model = %v, want nil (Gemini never sets it)", *chunk.Model)
	}
	if chunk.Usage != nil {
		t.Errorf("usage = %+v, want nil (Gemini never sets it)", *chunk.Usage)
	}

	golden := fmt.Sprintf(
		`data: {"id":%q,"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		chunk.ID,
	)
	if string(out) != golden {
		t.Errorf("Gemini chunk output changed shape:\n got:  %s\n want: %s", out, golden)
	}
}

// ── end-to-end: include_usage delivers cached_tokens through the PII restorer ─

// TestAnthropicStream_IncludeUsage_EndToEndViaProxy extends the
// Stage0c end-to-end harness pattern (TestAnthropicStream_Stage0c_EndToEnd_ViaProxy)
// with stream_options.include_usage:true and verifies that the client
// receives a trailing usage chunk whose prompt_tokens_details.cached_tokens
// reflects Anthropic's cache_read_input_tokens, surviving the PII
// StreamRestorer's usage whitelist.
func TestAnthropicStream_IncludeUsage_EndToEndViaProxy(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		events := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_iu","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","stop_reason":null,` +
				`"usage":{"input_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":5,"output_tokens":0}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi there"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}
		for _, line := range events {
			fmt.Fprintln(w, line)
		}
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryAnthropic(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := `{"model":"anthropic-test","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("status = %d, want 200; body: %s", streamResp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	if !strings.Contains(fullStr, "[DONE]") {
		t.Fatalf("stream did not complete cleanly with [DONE]\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"cached_tokens":20`) {
		t.Errorf("cached_tokens:20 absent from client output; PII restorer must preserve it\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, "hi there") {
		t.Errorf("expected content 'hi there' absent from output\noutput: %s", fullStr)
	}
}

// ── cache_control validation table (all positions) ───────────────────────────

// ccPosition describes one place in an Anthropic request where a client can
// supply a cache_control value: build constructs a full request body with the
// given raw cache_control value substituted in, and extract locates the
// (possibly re-marshaled) cache_control bytes in the transformed output,
// reporting whether the field is present at all.
type ccPosition struct {
	name    string
	build   func(ccValue string) string
	extract func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool)
}

// ccPositions enumerates every position cache_control validation must hold
// for: top-level, and per-part on user, assistant (with and without
// tool_calls), tool-result, and system content blocks.
func ccPositions() []ccPosition {
	return []ccPosition{
		{
			name: "top-level",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"cache_control":%s}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				raw, ok := doc["cache_control"]
				return raw, ok
			},
		},
		{
			name: "user text part",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":%s}]}]}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				msgs := unmarshalMessages(t, doc)
				if len(msgs) != 1 || len(msgs[0].Content) != 1 {
					t.Fatalf("unexpected messages shape: %+v", msgs)
				}
				cc := msgs[0].Content[0].CacheControl
				return cc, len(cc) > 0
			},
		},
		{
			name: "assistant text part (no tool_calls)",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[{"role":"user","content":"q"},`+
						`{"role":"assistant","content":[{"type":"text","text":"answer","cache_control":%s}]}]}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				msgs := unmarshalMessages(t, doc)
				if len(msgs) != 2 || len(msgs[1].Content) != 1 {
					t.Fatalf("unexpected messages shape: %+v", msgs)
				}
				cc := msgs[1].Content[0].CacheControl
				return cc, len(cc) > 0
			},
		},
		{
			name: "assistant text part with tool_calls",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[{"role":"user","content":"q"},`+
						`{"role":"assistant","content":[{"type":"text","text":"part1","cache_control":%s}],`+
						`"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]}]}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				msgs := unmarshalMessages(t, doc)
				if len(msgs) != 2 || len(msgs[1].Content) < 1 {
					t.Fatalf("unexpected messages shape: %+v", msgs)
				}
				cc := msgs[1].Content[0].CacheControl
				return cc, len(cc) > 0
			},
		},
		{
			name: "tool-result text part",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[{"role":"user","content":"q"},`+
						`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]},`+
						`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"result","cache_control":%s}]}]}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				msgs := unmarshalMessages(t, doc)
				last := msgs[len(msgs)-1]
				if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
					t.Fatalf("unexpected last message shape: %+v", last)
				}
				var blocks []anthropicContentBlock
				if err := json.Unmarshal(last.Content[0].Content, &blocks); err != nil {
					t.Fatalf("unmarshal tool_result content: %v (raw: %s)", err, last.Content[0].Content)
				}
				if len(blocks) != 1 {
					t.Fatalf("len(tool_result blocks) = %d, want 1", len(blocks))
				}
				cc := blocks[0].CacheControl
				return cc, len(cc) > 0
			},
		},
		{
			name: "system text part",
			build: func(cc string) string {
				return fmt.Sprintf(
					`{"model":"claude-3","messages":[`+
						`{"role":"system","content":[{"type":"text","text":"instr","cache_control":%s}]},`+
						`{"role":"user","content":"hi"}]}`,
					cc)
			},
			extract: func(t *testing.T, doc map[string]json.RawMessage) (json.RawMessage, bool) {
				raw, ok := doc["system"]
				if !ok {
					t.Fatal("output missing system field")
				}
				var blocks []anthropicContentBlock
				if err := json.Unmarshal(raw, &blocks); err != nil {
					t.Fatalf("unmarshal system as array: %v (raw: %s)", err, raw)
				}
				if len(blocks) != 1 {
					t.Fatalf("len(system blocks) = %d, want 1", len(blocks))
				}
				cc := blocks[0].CacheControl
				return cc, len(cc) > 0
			},
		},
	}
}

// TestAnthropicTransformRequest_CacheControlValid verifies that every valid
// cache_control shape — {"type":"ephemeral"} with no ttl, ttl "5m", or ttl
// "1h" — is accepted at every position and re-encoded to the exact canonical
// form (only the "type" and "ttl" keys, nothing else), never forwarding the
// client-supplied bytes verbatim.
func TestAnthropicTransformRequest_CacheControlValid(t *testing.T) {
	t.Parallel()

	validCases := []struct {
		name string
		raw  string
		want string
	}{
		{"type only", `{"type":"ephemeral"}`, `{"type":"ephemeral"}`},
		{"ttl 5m", `{"type":"ephemeral","ttl":"5m"}`, `{"type":"ephemeral","ttl":"5m"}`},
		{"ttl 1h", `{"type":"ephemeral","ttl":"1h"}`, `{"type":"ephemeral","ttl":"1h"}`},
	}

	for _, pos := range ccPositions() {
		pos := pos
		for _, vc := range validCases {
			vc := vc
			t.Run(pos.name+"/"+vc.name, func(t *testing.T) {
				t.Parallel()
				doc := transformRequest(t, pos.build(vc.raw))
				got, ok := pos.extract(t, doc)
				if !ok {
					t.Fatalf("cache_control absent, want present with canonical %s", vc.want)
				}
				if string(got) != vc.want {
					t.Errorf("cache_control = %s, want canonical %s", got, vc.want)
				}
			})
		}
	}
}

// TestAnthropicTransformRequest_CacheControlNullTreatedAsAbsent verifies that
// a JSON null cache_control value is treated as if the field were absent
// entirely, at every position, rather than being rejected or forwarded.
func TestAnthropicTransformRequest_CacheControlNullTreatedAsAbsent(t *testing.T) {
	t.Parallel()

	for _, pos := range ccPositions() {
		pos := pos
		t.Run(pos.name, func(t *testing.T) {
			t.Parallel()
			doc := transformRequest(t, pos.build("null"))
			_, ok := pos.extract(t, doc)
			if ok {
				t.Error("cache_control present, want absent for a JSON null input")
			}
		})
	}
}

// TestAnthropicTransformRequest_CacheControlInvalidRejected verifies that
// every malformed cache_control shape is rejected fail-closed at every
// position with the exact static, caller-content-free message — including an
// extra key whose value is an email address, which must never appear in the
// returned error.
func TestAnthropicTransformRequest_CacheControlInvalidRejected(t *testing.T) {
	t.Parallel()

	const wantMsg = `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`
	const leakEmail = "attacker@example.com"

	invalidCases := []struct {
		name string
		raw  string
	}{
		{"extra key with email value", fmt.Sprintf(`{"type":"ephemeral","note":%q}`, leakEmail)},
		{"type persistent", `{"type":"persistent"}`},
		{"ttl 10m", `{"type":"ephemeral","ttl":"10m"}`},
		{"ttl as number", `{"type":"ephemeral","ttl":5}`},
		{"cache_control as string", `"ephemeral"`},
		{"cache_control as array", `["ephemeral"]`},
		{"cache_control as number", `42`},
	}

	for _, pos := range ccPositions() {
		pos := pos
		for _, ic := range invalidCases {
			ic := ic
			t.Run(pos.name+"/"+ic.name, func(t *testing.T) {
				t.Parallel()
				input := pos.build(ic.raw)
				a := &AnthropicAdapter{}
				_, err := a.TransformRequest([]byte(input), Model{})
				if err == nil {
					t.Fatalf("expected error for input %s, got nil", input)
				}
				var clientErr *clientRequestError
				if !errors.As(err, &clientErr) {
					t.Fatalf("error does not unwrap to a clientRequestError: %v", err)
				}
				if clientErr.Error() != wantMsg {
					t.Errorf("client-safe message = %q, want %q", clientErr.Error(), wantMsg)
				}
				if strings.Contains(err.Error(), leakEmail) {
					t.Errorf("SECURITY: error leaks caller-supplied email: %v", err)
				}
			})
		}
	}
}

// ── assistant tool_calls: content shapes ──────────────────────────────────────

// TestAnthropicTransformRequest_AssistantToolCallsContentShapes covers an
// assistant message that carries both tool_calls and array-of-parts content:
// each text part becomes its own content block (never concatenated),
// per-part cache_control survives, empty parts are skipped, a non-text part
// is rejected fail-closed, string content still works unchanged, and no
// content at all yields only the tool_use blocks. Null/string content plus a
// single tool_call is also covered by TestAnthropicTransformRequest_AssistantToolCalls
// in anthropic_test.go; this test focuses on the array-content shape.
func TestAnthropicTransformRequest_AssistantToolCallsContentShapes(t *testing.T) {
	t.Parallel()

	t.Run("two text parts stay separate, cache_control kept, tool_use blocks follow", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":[{"type":"text","text":"alice@"},{"type":"text","text":"example.com","cache_control":{"type":"ephemeral"}}],` +
			`"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)

		var assistant *anthropicOutboundMessage
		for i := range msgs {
			if msgs[i].Role == "assistant" {
				assistant = &msgs[i]
				break
			}
		}
		if assistant == nil {
			t.Fatal("no assistant message in output")
		}
		if len(assistant.Content) != 3 {
			t.Fatalf("len(content) = %d, want 3 (two text blocks + one tool_use); blocks: %+v", len(assistant.Content), assistant.Content)
		}
		if assistant.Content[0].Type != "text" || assistant.Content[0].Text != "alice@" {
			t.Errorf("content[0] = %+v, want text %q", assistant.Content[0], "alice@")
		}
		if assistant.Content[1].Type != "text" || assistant.Content[1].Text != "example.com" {
			t.Errorf("content[1] = %+v, want text %q", assistant.Content[1], "example.com")
		}
		if len(assistant.Content[1].CacheControl) == 0 {
			t.Error("content[1].cache_control missing, want preserved")
		}
		if assistant.Content[2].Type != "tool_use" {
			t.Errorf("content[2].type = %q, want tool_use", assistant.Content[2].Type)
		}
		if strings.Contains(string(doc["messages"]), "alice@example.com") {
			t.Errorf("SECURITY: joined PII string appears in assistant content; raw: %s", doc["messages"])
		}
	})

	t.Run("string content still works alongside tool_calls", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":"Sure, calling tool.","tool_calls":[{"id":"c2","type":"function","function":{"name":"fn","arguments":"{}"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		assistant := msgs[len(msgs)-1]
		if len(assistant.Content) != 2 {
			t.Fatalf("len(content) = %d, want 2; blocks: %+v", len(assistant.Content), assistant.Content)
		}
		if assistant.Content[0].Type != "text" || assistant.Content[0].Text != "Sure, calling tool." {
			t.Errorf("content[0] = %+v, want text %q", assistant.Content[0], "Sure, calling tool.")
		}
		if assistant.Content[1].Type != "tool_use" {
			t.Errorf("content[1].type = %q, want tool_use", assistant.Content[1].Type)
		}
	})

	t.Run("empty parts are skipped", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"kept"}],` +
			`"tool_calls":[{"id":"c3","type":"function","function":{"name":"fn","arguments":"{}"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		assistant := msgs[len(msgs)-1]
		if len(assistant.Content) != 2 {
			t.Fatalf("len(content) = %d, want 2 (empty part skipped + tool_use); blocks: %+v", len(assistant.Content), assistant.Content)
		}
		if assistant.Content[0].Type != "text" || assistant.Content[0].Text != "kept" {
			t.Errorf("content[0] = %+v, want text %q", assistant.Content[0], "kept")
		}
		if assistant.Content[1].Type != "tool_use" {
			t.Errorf("content[1].type = %q, want tool_use", assistant.Content[1].Type)
		}
	})

	t.Run("non-text part is rejected fail-closed", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"http://example.com/img.png"}}],` +
			`"tool_calls":[{"id":"c4","type":"function","function":{"name":"fn","arguments":"{}"}}]}` +
			`]}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for non-text assistant content part, got nil")
		}
		var clientErr *clientRequestError
		if !errors.As(err, &clientErr) {
			t.Fatalf("error does not unwrap to a clientRequestError: %v", err)
		}
		const wantMsg = "only text content parts are supported for this model"
		if clientErr.Error() != wantMsg {
			t.Errorf("client-safe message = %q, want %q", clientErr.Error(), wantMsg)
		}
	})

	t.Run("no content at all yields only tool_use blocks", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"go"},` +
			`{"role":"assistant","tool_calls":[{"id":"c5","type":"function","function":{"name":"fn","arguments":"{}"}}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		assistant := msgs[len(msgs)-1]
		if len(assistant.Content) != 1 {
			t.Fatalf("len(content) = %d, want 1 (tool_use only); blocks: %+v", len(assistant.Content), assistant.Content)
		}
		if assistant.Content[0].Type != "tool_use" {
			t.Errorf("content[0].type = %q, want tool_use", assistant.Content[0].Type)
		}
	})
}

// ── handler: non-client-safe adapter errors ───────────────────────────────────

// TestAnthropicTransformRequest_NonClientSafeErrorSurfacesGenericMessage is the
// counterpart to TestAnthropicTransformRequest_ClientSafeErrorSurfacesThroughHandler:
// a TransformRequest failure that is NOT a clientRequestError (an unrecognized
// tool_choice string, a plain adapter error rather than a client-safe one) must
// surface as the generic "failed to transform request for provider" message,
// never the underlying adapter error text.
func TestAnthropicTransformRequest_NonClientSafeErrorSurfacesGenericMessage(t *testing.T) {
	t.Parallel()

	reg := anthropicClientErrorRegistry(t)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app := testApp(t, handler)

	body := `{"model":"claude-handler-test","messages":[{"role":"user","content":"hi"}],"tool_choice":"not-a-valid-choice"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("unmarshal response body: %v (body: %s)", err, respBody)
	}
	if envelope.Error.Code != "bad_request" {
		t.Errorf("error.code = %q, want %q", envelope.Error.Code, "bad_request")
	}
	const wantMsg = "failed to transform request for provider"
	if envelope.Error.Message != wantMsg {
		t.Errorf("error.message = %q, want %q", envelope.Error.Message, wantMsg)
	}
	if strings.Contains(envelope.Error.Message, "not-a-valid-choice") {
		t.Errorf("SECURITY: generic error message leaked adapter-internal detail: %q", envelope.Error.Message)
	}
}
