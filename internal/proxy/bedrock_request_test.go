package proxy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// bedrockXform runs TransformRequest and decodes the upstream body.
func bedrockXform(t *testing.T, body string) (map[string]json.RawMessage, *BedrockAdapter) {
	t.Helper()
	a := &BedrockAdapter{}
	out, err := a.TransformRequest([]byte(body), Model{})
	if err != nil {
		t.Fatalf("TransformRequest: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("upstream body is not JSON: %v\n%s", err, out)
	}
	for k := range doc {
		switch k {
		case "messages", "system", "inferenceConfig", "toolConfig":
		default:
			t.Errorf("unexpected top-level key %q in upstream body", k)
		}
	}
	return doc, a
}

// assertJSONEq compares got against the wantJSON document structurally.
func assertJSONEq(t *testing.T, label string, got json.RawMessage, wantJSON string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Errorf("%s: got is not JSON (%q): %v", label, got, err)
		return
	}
	if err := json.Unmarshal([]byte(wantJSON), &w); err != nil {
		t.Fatalf("%s: bad want JSON: %v", label, err)
	}
	if !reflect.DeepEqual(g, w) {
		gb, _ := json.Marshal(g)
		wb, _ := json.Marshal(w)
		t.Errorf("%s:\n got  %s\n want %s", label, gb, wb)
	}
}

func TestBedrockAdapter_RequestMapping(t *testing.T) {
	t.Parallel()

	const u = `{"role":"user","content":"hi"}`
	tests := []struct {
		name string
		body string
		// want maps a top-level upstream key to its expected JSON.
		want map[string]string
		// absent lists top-level keys that must not be present.
		absent []string
	}{
		{
			name: "system string",
			body: `{"messages":[{"role":"system","content":"be nice"},` + u + `]}`,
			want: map[string]string{
				"system":   `[{"text":"be nice"}]`,
				"messages": `[{"role":"user","content":[{"text":"hi"}]}]`,
			},
			absent: []string{"inferenceConfig", "toolConfig"},
		},
		{
			name: "system parts never joined and empty skipped",
			body: `{"messages":[{"role":"system","content":[{"type":"text","text":"a"},{"type":"text","text":""},{"type":"text","text":"b"}]},` + u + `]}`,
			want: map[string]string{"system": `[{"text":"a"},{"text":"b"}]`},
		},
		{
			name: "developer role maps to system",
			body: `{"messages":[{"role":"developer","content":"dev"},` + u + `]}`,
			want: map[string]string{"system": `[{"text":"dev"}]`},
		},
		{
			name: "multiple system messages stay separate blocks",
			body: `{"messages":[{"role":"system","content":"s1"},{"role":"system","content":"s2"},` + u + `]}`,
			want: map[string]string{"system": `[{"text":"s1"},{"text":"s2"}]`},
		},
		{
			name:   "system with null content contributes nothing",
			body:   `{"messages":[{"role":"system","content":null},` + u + `]}`,
			want:   map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]}]`},
			absent: []string{"system"},
		},
		{
			name: "user parts one block each",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":""},{"type":"text","text":"b"}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"},{"text":"b"}]}]`},
		},
		{
			name: "special characters preserved",
			body: `{"messages":[{"role":"user","content":"<a href=\"x\">&amp; \u00e4\n</a>"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"<a href=\"x\">&amp; \u00e4\n</a>"}]}]`},
		},
		{
			name: "assistant text then tool calls",
			body: `{"messages":[` + u + `,{"role":"assistant","content":"let me","tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":[1,2]}"}},` +
				`{"id":"c2","type":"function","function":{"name":"g","arguments":""}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]},` +
				`{"role":"assistant","content":[{"text":"let me"},` +
				`{"toolUse":{"toolUseId":"c1","name":"f","input":{"a":[1,2]}}},` +
				`{"toolUse":{"toolUseId":"c2","name":"g","input":{}}}]}]`},
		},
		{
			name: "assistant tool calls only with null content",
			body: `{"messages":[` + u + `,{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]},{"role":"assistant","content":[{"toolUse":{"toolUseId":"c1","name":"f","input":{}}}]}]`},
		},
		{
			name: "tool result string",
			body: `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"c1","content":"42"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]},` +
				`{"role":"assistant","content":[{"toolUse":{"toolUseId":"c1","name":"f","input":{}}}]},` +
				`{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"text":"42"}]}}]}]`},
		},
		{
			name: "tool result parts keep boundaries",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"text":"a"},{"text":"b"}]}}]}]`},
		},
		{
			name: "tool result empty string becomes empty json object",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":""}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"json":{}}]}}]}]`},
		},
		{
			name: "tool result null content becomes empty json object",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":null}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"json":{}}]}}]}]`},
		},
		{
			name: "tool result missing content becomes empty json object",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"json":{}}]}}]}]`},
		},
		{
			name: "tool result empty parts becomes empty json object",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":""}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c1","content":[{"json":{}}]}}]}]`},
		},
		{
			name: "two consecutive tool results share one user turn",
			body: `{"messages":[` + u + `,{"role":"assistant","tool_calls":[` +
				`{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},` +
				`{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"a","content":"1"},{"role":"tool","tool_call_id":"b","content":"2"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]},` +
				`{"role":"assistant","content":[{"toolUse":{"toolUseId":"a","name":"f","input":{}}},{"toolUse":{"toolUseId":"b","name":"f","input":{}}}]},` +
				`{"role":"user","content":[{"toolResult":{"toolUseId":"a","content":[{"text":"1"}]}},{"toolResult":{"toolUseId":"b","content":[{"text":"2"}]}}]}]`},
		},
		{
			name: "tool result merges into following user turn",
			body: `{"messages":[{"role":"tool","tool_call_id":"a","content":"1"},{"role":"user","content":"next"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"a","content":[{"text":"1"}]}},{"text":"next"}]}]`},
		},
		{
			name: "tool result merges into preceding user turn",
			body: `{"messages":[{"role":"user","content":"pre"},{"role":"tool","tool_call_id":"a","content":"1"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"pre"},{"toolResult":{"toolUseId":"a","content":[{"text":"1"}]}}]}]`},
		},
		{
			name: "consecutive same role merged without concatenation",
			body: `{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"},{"role":"assistant","content":"c"},{"role":"assistant","content":"d"}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"},{"text":"b"}]},{"role":"assistant","content":[{"text":"c"},{"text":"d"}]}]`},
		},
		{
			name: "system between users does not split the merge",
			body: `{"messages":[{"role":"user","content":"a"},{"role":"system","content":"s"},{"role":"user","content":"b"}]}`,
			want: map[string]string{
				"messages": `[{"role":"user","content":[{"text":"a"},{"text":"b"}]}]`,
				"system":   `[{"text":"s"}]`,
			},
		},
		{
			name: "cache_control 1h on user part",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"b"}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"},{"cachePoint":{"type":"default","ttl":"1h"}},{"text":"b"}]}]`},
		},
		{
			name: "cache_control 5m has no ttl",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"},{"cachePoint":{"type":"default"}}]}]`},
		},
		{
			name: "cache_control without ttl has no ttl",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"},{"cachePoint":{"type":"default"}}]}]`},
		},
		{
			name: "cache_control null adds nothing",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":null}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"a"}]}]`},
		},
		{
			name: "cache_control on system part",
			body: `{"messages":[{"role":"system","content":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"role":"user","content":"u"}]}`,
			want: map[string]string{"system": `[{"text":"s"},{"cachePoint":{"type":"default","ttl":"1h"}}]`},
		},
		{
			name: "cache_control on assistant part precedes tool calls",
			body: `{"messages":[{"role":"user","content":"u"},{"role":"assistant","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}],"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"u"}]},{"role":"assistant","content":[{"text":"a"},{"cachePoint":{"type":"default"}},{"toolUse":{"toolUseId":"c","name":"f","input":{}}}]}]`},
		},
		{
			name: "cache_control on tool result part gets no cachePoint",
			body: `{"messages":[{"role":"tool","tool_call_id":"c","content":[{"type":"text","text":"r","cache_control":{"type":"ephemeral"}}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"toolResult":{"toolUseId":"c","content":[{"text":"r"}]}}]}]`},
		},
		{
			name: "cache_control on empty text part adds no block",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"","cache_control":{"type":"ephemeral"}},{"type":"text","text":"x"}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"x"}]}]`},
		},
		{
			name: "max_tokens",
			body: `{"max_tokens":128,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":128}`},
		},
		{
			name: "max_completion_tokens used when max_tokens absent",
			body: `{"max_completion_tokens":64,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":64}`},
		},
		{
			name: "max_tokens wins over max_completion_tokens",
			body: `{"max_tokens":10,"max_completion_tokens":64,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":10}`},
		},
		{
			name: "max_tokens null falls back to max_completion_tokens",
			body: `{"max_tokens":null,"max_completion_tokens":64,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":64}`},
		},
		{
			name: "integral float max_tokens",
			body: `{"max_tokens":100.0,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":100}`},
		},
		{
			name:   "both max token fields null inject no default",
			body:   `{"max_tokens":null,"max_completion_tokens":null,"messages":[` + u + `]}`,
			absent: []string{"inferenceConfig"},
		},
		{
			name: "temperature in range",
			body: `{"temperature":0.25,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"temperature":0.25}`},
		},
		{
			name: "temperature zero is sent",
			body: `{"temperature":0,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"temperature":0}`},
		},
		{
			name: "temperature above one is clamped",
			body: `{"temperature":1.7,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"temperature":1}`},
		},
		{
			name:   "temperature null absent",
			body:   `{"temperature":null,"messages":[` + u + `]}`,
			absent: []string{"inferenceConfig"},
		},
		{
			name: "top_p",
			body: `{"top_p":0.9,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"topP":0.9}`},
		},
		{
			name: "top_p boundaries",
			body: `{"top_p":1,"temperature":0,"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"topP":1,"temperature":0}`},
		},
		{
			name: "stop string",
			body: `{"stop":"END","messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"stopSequences":["END"]}`},
		},
		{
			name: "stop array drops blanks",
			body: `{"stop":["a"," ","b"],"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"stopSequences":["a","b"]}`},
		},
		{
			name:   "stop empty string absent",
			body:   `{"stop":"","messages":[` + u + `]}`,
			absent: []string{"inferenceConfig"},
		},
		{
			name:   "stop null absent",
			body:   `{"stop":null,"messages":[` + u + `]}`,
			absent: []string{"inferenceConfig"},
		},
		{
			name: "all sampling params together",
			body: `{"max_tokens":5,"temperature":0.5,"top_p":0.5,"stop":["x"],"messages":[` + u + `]}`,
			want: map[string]string{"inferenceConfig": `{"maxTokens":5,"temperature":0.5,"topP":0.5,"stopSequences":["x"]}`},
		},
		{
			name: "tool with default schema",
			body: `{"tools":[{"type":"function","function":{"name":"f"}}],"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}]}`},
		},
		{
			name: "tool with description and parameters and no type",
			body: `{"tools":[{"function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}],"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","description":"d","inputSchema":{"json":{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}}}}]}`},
		},
		{
			name: "tool parameters null uses default schema",
			body: `{"tools":[{"type":"function","function":{"name":"f","parameters":null}}],"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}]}`},
		},
		{
			name:   "empty tools array no toolConfig",
			body:   `{"tools":[],"tool_choice":"auto","messages":[` + u + `]}`,
			absent: []string{"toolConfig"},
		},
		{
			name:   "tools null no toolConfig",
			body:   `{"tools":null,"messages":[` + u + `]}`,
			absent: []string{"toolConfig"},
		},
		{
			name: "tool_choice auto",
			body: `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto","messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}],"toolChoice":{"auto":{}}}`},
		},
		{
			name: "tool_choice required maps to any",
			body: `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"required","messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}],"toolChoice":{"any":{}}}`},
		},
		{
			name: "tool_choice named",
			body: `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"f"}},"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}],"toolChoice":{"tool":{"name":"f"}}}`},
		},
		{
			name:   "tool_choice none drops toolConfig",
			body:   `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none","messages":[` + u + `]}`,
			absent: []string{"toolConfig"},
		},
		{
			name: "tool_choice null is absent",
			body: `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":null,"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}]}`},
		},
		{
			name:   "tool_choice without tools is ignored",
			body:   `{"tool_choice":"required","messages":[` + u + `]}`,
			absent: []string{"toolConfig"},
		},
		{
			name: "parallel_tool_calls bool accepted and dropped",
			body: `{"parallel_tool_calls":false,"tools":[{"type":"function","function":{"name":"f"}}],"messages":[` + u + `]}`,
			want: map[string]string{"toolConfig": `{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object","properties":{}}}}}]}`},
		},
		{
			name: "unknown and native fields dropped silently",
			body: `{"n":3,"response_format":{"type":"json_object"},"user":"u","metadata":{"a":1},"logprobs":true,"top_logprobs":2,"seed":1,` +
				`"top_k":5,"thinking":{"type":"enabled"},"reasoning_effort":"high","cache_control":{"type":"ephemeral"},"functions":[{"name":"f"}],` +
				`"function_call":"auto","inferenceConfig":{"maxTokens":99999},"toolConfig":{"tools":[]},"additionalModelRequestFields":{"x":1},` +
				`"guardrailConfig":{"guardrailIdentifier":"g"},"requestMetadata":{"a":"b"},"promptVariables":{"a":{"text":"b"}},` +
				`"performanceConfig":{"latency":"optimized"},"additionalModelResponseFieldPaths":["/x"],"frequency_penalty":1,"presence_penalty":1,` +
				`"messages":[` + u + `]}`,
			want:   map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]}]`},
			absent: []string{"inferenceConfig", "toolConfig", "system"},
		},
		{
			name: "per message extra fields dropped",
			body: `{"messages":[{"role":"user","content":"hi","name":"bob","refusal":"x","audio":{"id":"1"},"extra":1}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[{"text":"hi"}]}]`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, _ := bedrockXform(t, tc.body)
			for k, w := range tc.want {
				raw, ok := doc[k]
				if !ok {
					t.Errorf("key %q missing", k)
					continue
				}
				assertJSONEq(t, k, raw, w)
			}
			for _, k := range tc.absent {
				if _, ok := doc[k]; ok {
					t.Errorf("key %q must be absent, got %s", k, doc[k])
				}
			}
		})
	}
}

func TestBedrockAdapter_RequestState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantStreaming bool
		wantUsage     bool
		wantModel     string
	}{
		{"defaults", `{"model":"m","messages":[{"role":"user","content":"a"}]}`, false, false, "m"},
		{"stream true", `{"model":"m","stream":true,"messages":[{"role":"user","content":"a"}]}`, true, false, "m"},
		{"stream false", `{"model":"m","stream":false,"messages":[{"role":"user","content":"a"}]}`, false, false, "m"},
		{"stream null", `{"model":"m","stream":null,"messages":[{"role":"user","content":"a"}]}`, false, false, "m"},
		{"include_usage", `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"a"}]}`, true, true, "m"},
		{"include_usage false", `{"model":"m","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"a"}]}`, true, false, "m"},
		{"no model", `{"messages":[{"role":"user","content":"a"}]}`, false, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc, a := bedrockXform(t, tc.body)
			if a.streaming != tc.wantStreaming || a.includeUsage != tc.wantUsage || a.modelName != tc.wantModel {
				t.Errorf("state = streaming:%v usage:%v model:%q", a.streaming, a.includeUsage, a.modelName)
			}
			if !strings.HasPrefix(a.responseID, "chatcmpl-") {
				t.Errorf("responseID = %q, want chatcmpl- prefix", a.responseID)
			}
			if _, ok := doc["stream"]; ok {
				t.Error("stream must never be sent upstream")
			}
			if _, ok := doc["model"]; ok {
				t.Error("model must never be sent upstream")
			}
		})
	}
}

func TestBedrockAdapter_RequestRejections(t *testing.T) {
	t.Parallel()

	const u = `{"role":"user","content":"hi"}`
	tests := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"messages missing", `{"model":"m"}`, "messages must be a non-empty array"},
		{"messages empty", `{"messages":[]}`, "messages must be a non-empty array"},
		{"messages null", `{"messages":null}`, "messages must be a non-empty array"},
		{"messages not array", `{"messages":"x"}`, "messages must be a non-empty array"},
		{"top-level system", `{"system":"x","messages":[` + u + `]}`, `top-level system is not supported; send a message with role "system" instead`},
		{"top-level system null", `{"system":null,"messages":[` + u + `]}`, `top-level system is not supported; send a message with role "system" instead`},
		{"stop_sequences", `{"stop_sequences":["x"],"messages":[` + u + `]}`, "stop_sequences is not supported; use stop instead"},
		{"only system", `{"messages":[{"role":"system","content":"x"}]}`, "messages must contain a user or assistant message"},
		{"unknown role", `{"messages":[{"role":"function","content":"x"}]}`, "unsupported message role"},
		{"empty role", `{"messages":[{"content":"x"}]}`, "unsupported message role"},
		{"empty user string", `{"messages":[{"role":"user","content":""}]}`, "message content must not be empty"},
		{"null user content", `{"messages":[{"role":"user","content":null}]}`, "message content must not be empty"},
		{"user empty parts", `{"messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`, "message content must not be empty"},
		{"assistant empty", `{"messages":[` + u + `,{"role":"assistant","content":""}]}`, "message content must not be empty"},
		{"image part", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "only text content parts are supported for this model"},
		{"audio part", `{"messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`, "only text content parts are supported for this model"},
		{"file part", `{"messages":[{"role":"user","content":[{"type":"file"}]}]}`, "only text content parts are supported for this model"},
		{"system non-text part", `{"messages":[{"role":"system","content":[{"type":"image_url"}]},` + u + `]}`, "only text content parts are supported for this model"},
		{"tool result non-text part", `{"messages":[{"role":"tool","tool_call_id":"c","content":[{"type":"image_url"}]}]}`, "only text content parts are supported for this model"},
		{"numeric content", `{"messages":[{"role":"user","content":5}]}`, "unsupported message content"},
		{"numeric system content", `{"messages":[{"role":"system","content":5},` + u + `]}`, "unsupported message content"},
		{"invalid cache_control type", `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"persistent"}}]}]}`, `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`},
		{"invalid cache_control ttl", `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"2h"}}]}]}`, `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`},
		{"invalid cache_control extra key", `{"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","x":1}}]}]}`, `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`},
		{"invalid cache_control on system", `{"messages":[{"role":"system","content":[{"type":"text","text":"a","cache_control":"x"}]},` + u + `]}`, `cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`},
		{"tool call id missing", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, "tool_call id is missing or invalid"},
		{"tool call id bad charset", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"a b","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, "tool_call id is missing or invalid"},
		{"tool call id too long", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"` + strings.Repeat("a", 65) + `","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, "tool_call id is missing or invalid"},
		{"tool call name bad", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f.g","arguments":"{}"}}]}]}`, "tool_call function name is invalid"},
		{"tool call name empty", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"","arguments":"{}"}}]}]}`, "tool_call function name is invalid"},
		{"tool call args array", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"[1]"}}]}]}`, "tool_call arguments must be a JSON object"},
		{"tool call args invalid", `{"messages":[` + u + `,{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{"}}]}]}`, "tool_call arguments must be a JSON object"},
		{"tool_call_id missing", `{"messages":[{"role":"tool","content":"x"}]}`, "tool_call_id is missing or invalid"},
		{"tool_call_id bad", `{"messages":[{"role":"tool","tool_call_id":"a b","content":"x"}]}`, "tool_call_id is missing or invalid"},
		{"max_tokens zero", `{"max_tokens":0,"messages":[` + u + `]}`, "max_tokens must be a positive integer"},
		{"max_tokens negative", `{"max_tokens":-5,"messages":[` + u + `]}`, "max_tokens must be a positive integer"},
		{"max_tokens string", `{"max_tokens":"5","messages":[` + u + `]}`, "max_tokens must be a positive integer"},
		{"max_tokens fractional", `{"max_tokens":1.5,"messages":[` + u + `]}`, "max_tokens must be a positive integer"},
		{"max_completion_tokens zero", `{"max_completion_tokens":0,"messages":[` + u + `]}`, "max_tokens must be a positive integer"},
		{"temperature negative", `{"temperature":-0.1,"messages":[` + u + `]}`, "temperature must be between 0 and 2"},
		{"temperature string", `{"temperature":"hot","messages":[` + u + `]}`, "temperature must be a number"},
		{"top_p above one", `{"top_p":1.5,"messages":[` + u + `]}`, "top_p must be between 0 and 1"},
		{"top_p negative", `{"top_p":-0.1,"messages":[` + u + `]}`, "top_p must be between 0 and 1"},
		{"top_p string", `{"top_p":"x","messages":[` + u + `]}`, "top_p must be a number"},
		{"stop number", `{"stop":5,"messages":[` + u + `]}`, "stop must be a string or an array of strings"},
		{"stop array with number", `{"stop":["a",1],"messages":[` + u + `]}`, "stop must be a string or an array of strings"},
		{"stream string", `{"stream":"yes","messages":[` + u + `]}`, "stream must be a boolean"},
		{"stream number", `{"stream":1,"messages":[` + u + `]}`, "stream must be a boolean"},
		{"parallel_tool_calls string", `{"parallel_tool_calls":"no","messages":[` + u + `]}`, "parallel_tool_calls must be a boolean"},
		{"parallel_tool_calls null", `{"parallel_tool_calls":null,"messages":[` + u + `]}`, "parallel_tool_calls must be a boolean"},
		{"tools not array", `{"tools":"x","messages":[` + u + `]}`, "tools must be an array of function tools"},
		{"tool type unsupported", `{"tools":[{"type":"retrieval","function":{"name":"f"}}],"messages":[` + u + `]}`, "unsupported tool type"},
		{"tool name invalid", `{"tools":[{"type":"function","function":{"name":"bad name"}}],"messages":[` + u + `]}`, "tool function name is invalid"},
		{"tool name empty", `{"tools":[{"type":"function","function":{}}],"messages":[` + u + `]}`, "tool function name is invalid"},
		{"tool name too long", `{"tools":[{"type":"function","function":{"name":"` + strings.Repeat("n", 65) + `"}}],"messages":[` + u + `]}`, "tool function name is invalid"},
		{"tool parameters array", `{"tools":[{"type":"function","function":{"name":"f","parameters":[1]}}],"messages":[` + u + `]}`, "tool parameters must be a JSON object"},
		{"tool parameters string", `{"tools":[{"type":"function","function":{"name":"f","parameters":"x"}}],"messages":[` + u + `]}`, "tool parameters must be a JSON object"},
		{"tool_choice unknown string", `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"sometimes","messages":[` + u + `]}`, "tool_choice is invalid"},
		{"tool_choice undeclared", `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"g"}},"messages":[` + u + `]}`, "tool_choice is invalid"},
		{"tool_choice bad object", `{"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"other"},"messages":[` + u + `]}`, "tool_choice is invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := (&BedrockAdapter{}).TransformRequest([]byte(tc.body), Model{})
			if err == nil {
				t.Fatal("want error, got nil")
			}
			var cre *clientRequestError
			if !errors.As(err, &cre) {
				t.Fatalf("error %T (%v) is not a clientRequestError", err, err)
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("message = %q, want %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestBedrockAdapter_RequestMalformedBody(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `{`, `[]`, `"x"`, `null`, `not json`} {
		_, err := (&BedrockAdapter{}).TransformRequest([]byte(body), Model{})
		if err == nil {
			t.Errorf("%q: want error", body)
		}
	}
}

// TestBedrockAdapter_RequestErrorsNeverEchoContent proves rejected requests do
// not reflect caller-supplied text in the client-visible error.
func TestBedrockAdapter_RequestErrorsNeverEchoContent(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-PROMPT-TEXT"
	bodies := []string{
		`{"messages":[{"role":"user","content":[{"type":"` + secret + `","text":"` + secret + `"}]}]}`,
		`{"messages":[{"role":"` + secret + `","content":"` + secret + `"}]}`,
		`{"messages":[{"role":"user","content":"` + secret + `"},{"role":"assistant","tool_calls":[{"id":"` + secret + ` x","type":"function","function":{"name":"f","arguments":"` + secret + `"}}]}]}`,
		`{"messages":[{"role":"user","content":"` + secret + `"}],"tools":[{"type":"function","function":{"name":"` + secret + ` bad"}}]}`,
		`{"messages":[{"role":"user","content":"` + secret + `"}],"temperature":"` + secret + `"}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"` + secret + `","cache_control":{"type":"` + secret + `"}}]}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"` + secret + ` bad","content":"` + secret + `"}]}`,
		`{"messages":[{"role":"user","content":"` + secret + `"}],"tool_choice":"` + secret + `","tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"messages":[{"role":"user","content":"` + secret + `"}],"stop":[1,"` + secret + `"]}`,
		secret,
	}
	for i, body := range bodies {
		_, err := (&BedrockAdapter{}).TransformRequest([]byte(body), Model{})
		if err == nil {
			t.Errorf("body %d: want error", i)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("body %d: error echoes request content: %q", i, err.Error())
		}
	}
}

func TestBedrockAdapter_RequestToolInputPreservedExactly(t *testing.T) {
	t.Parallel()
	doc, _ := bedrockXform(t, `{"messages":[{"role":"user","content":"u"},{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":" {\"n\":1.50,\"s\":\"\\u00e4<>\",\"o\":{\"z\":null}} "}}]}]}`)
	var msgs []struct {
		Content []struct {
			ToolUse *struct {
				Input json.RawMessage `json:"input"`
			} `json:"toolUse"`
		} `json:"content"`
	}
	if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
		t.Fatal(err)
	}
	in := msgs[1].Content[0].ToolUse.Input
	var got, want any
	_ = json.Unmarshal(in, &got)
	_ = json.Unmarshal([]byte(`{"n":1.5,"s":"ä<>","o":{"z":null}}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool input = %s", in)
	}
}

func TestBedrockAdapter_RequestToolsManyAndOrder(t *testing.T) {
	t.Parallel()
	var tools []string
	for _, n := range []string{"zeta", "alpha", "mid-1", "Mid_2"} {
		tools = append(tools, `{"type":"function","function":{"name":"`+n+`"}}`)
	}
	doc, _ := bedrockXform(t, `{"tools":[`+strings.Join(tools, ",")+`],"messages":[{"role":"user","content":"u"}]}`)
	var tc struct {
		Tools []struct {
			ToolSpec struct {
				Name string `json:"name"`
			} `json:"toolSpec"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(doc["toolConfig"], &tc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tc.Tools {
		names = append(names, tl.ToolSpec.Name)
	}
	if !reflect.DeepEqual(names, []string{"zeta", "alpha", "mid-1", "Mid_2"}) {
		t.Errorf("tool order = %v", names)
	}
}
