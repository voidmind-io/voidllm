package pii

// function_name_test.go covers the shared function/tool-name policy
// (functionNamePattern + validateFunctionName) applied at every place a
// function/tool name appears in a request body: tools[].function.name,
// tool_choice.function.name, messages[].tool_calls[].function.name,
// messages[].function_call.name, and the legacy top-level functions[].name
// and function_call.name. It follows the conventions in pii_test.go and
// unknown_fields_test.go: newTestFilter/mustAnonymize helpers, white-box
// package pii, and the fixed errAnonymizeMsg for fail-closed assertions.

import (
	"encoding/json"
	"strings"
	"testing"
)

// functionNameLocation describes one place in a request body where a
// function/tool name is validated by the shared function-name policy, and
// how to build a request body embedding a given (already JSON-encoded) name
// at that location.
type functionNameLocation struct {
	// desc names the location for subtest names.
	desc string
	// body returns a full request body with nameJSON (a JSON-encoded string,
	// including its surrounding quotes) spliced in at this location's name
	// field.
	body func(nameJSON string) string
}

// functionNameLocations enumerates the four locations the task requires
// (tools[].function.name, tool_choice.function.name,
// messages[].tool_calls[].function.name, messages[].function_call.name)
// plus the two legacy top-level locations (functions[].name,
// function_call.name) that fell into the default unknown-field scan before
// this policy existed.
var functionNameLocations = []functionNameLocation{
	{
		desc: "tools[].function.name",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":` + n + `,"description":"looks up the weather"}}]}`
		},
	},
	{
		desc: "tool_choice.function.name",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":` + n + `}}}`
		},
	},
	{
		desc: "messages[].tool_calls[].function.name",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":` + n + `,"arguments":"{}"}}]}]}`
		},
	},
	{
		desc: "messages[].function_call.name",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"assistant","function_call":{"name":` + n + `,"arguments":"{}"}}]}`
		},
	},
	{
		desc: "top-level functions[].name (legacy)",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":` + n + `,"description":"looks up the weather"}]}`
		},
	},
	{
		desc: "top-level function_call.name (legacy)",
		body: func(n string) string {
			return `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"function_call":{"name":` + n + `}}`
		},
	},
}

// jsonString JSON-encodes s (including surrounding quotes) for splicing into
// the templates above. None of the names used in this file need anything
// beyond what encoding/json already produces.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ── valid names: accepted, byte-identical, never scanned ────────────────────

// TestFilter_FunctionName_ValidNamesUnchanged verifies that names matching
// functionNamePattern and carrying no PII are accepted at every location:
// the output is byte-identical to the input and Touched() reports false,
// proving the name was never passed to replace().
func TestFilter_FunctionName_ValidNamesUnchanged(t *testing.T) {
	t.Parallel()

	validNames := []string{"get_weather", "weather.lookup", "tool+v2"}

	for _, loc := range functionNameLocations {
		loc := loc
		for _, name := range validNames {
			name := name
			t.Run(loc.desc+"/"+name, func(t *testing.T) {
				t.Parallel()

				body := loc.body(jsonString(name))
				f := newTestFilter(t)
				out := mustAnonymize(t, f, []byte(body))

				if f.Touched() {
					t.Errorf("Touched() = true, want false: valid name %q must not be scanned; body: %s", name, out)
				}
				if string(out) != body {
					t.Errorf("AnonymizeJSON(%s) = %s, want byte-identical to input %s", loc.desc, out, body)
				}
			})
		}
	}
}

// ── invalid names: fail closed, never pseudonymized ──────────────────────────

// TestFilter_FunctionName_InvalidNamesFailClosed verifies that a name which
// looks like PII (email, credit card, phone number), contains a character
// outside functionNamePattern (a space), or exceeds the 128-character bound
// is rejected fail-closed at every location — never pseudonymized, because a
// declared function name and its later selection must stay identical.
func TestFilter_FunctionName_InvalidNamesFailClosed(t *testing.T) {
	t.Parallel()

	invalidNames := []struct {
		desc string
		name string
	}{
		{"email", "alice@example.com"},
		{"credit card", "4111111111111111"},
		{"phone number", rerankPhone},
		{"contains a space", "get weather"},
		{"129 characters", strings.Repeat("a", 129)},
	}

	for _, loc := range functionNameLocations {
		loc := loc
		for _, tc := range invalidNames {
			tc := tc
			t.Run(loc.desc+"/"+tc.desc, func(t *testing.T) {
				t.Parallel()

				body := loc.body(jsonString(tc.name))
				f := newTestFilter(t)
				_, err := f.AnonymizeJSON([]byte(body))
				if err == nil {
					t.Fatalf("expected fail-closed error for %s at %s, got nil", tc.desc, loc.desc)
				}
				if err.Error() != errAnonymizeMsg {
					t.Errorf("error = %q, want static message %q", err.Error(), errAnonymizeMsg)
				}
			})
		}
	}
}

// ── tool_choice + tools together: byte-identical when both names are valid ──

// TestFilter_FunctionName_ToolChoiceAndToolsBothValid_ByteIdentical verifies
// that a body carrying both a "tools" entry and a matching "tool_choice"
// selection — the shape a real client sends to force a specific tool call —
// is returned completely byte-identical when both names are valid and
// nothing else in the body carries PII.
func TestFilter_FunctionName_ToolChoiceAndToolsBothValid_ByteIdentical(t *testing.T) {
	t.Parallel()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"looks up the weather"}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`

	f := newTestFilter(t)
	out := mustAnonymize(t, f, []byte(body))

	if f.Touched() {
		t.Errorf("Touched() = true, want false: both function names are valid and carry no PII; body: %s", out)
	}
	if string(out) != body {
		t.Errorf("AnonymizeJSON(tools+tool_choice) = %s, want byte-identical to input %s", out, body)
	}
}

// ── null / absent name: each location keeps its own prior behavior ──────────

// TestFilter_FunctionName_AbsentName_NoOp verifies that when the name field
// is entirely absent, every location is a no-op — the shared policy is only
// applied when a name is actually present.
func TestFilter_FunctionName_AbsentName_NoOp(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		desc string
		body string
	}{
		{
			desc: "tools[].function without name",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"description":"d"}}]}`,
		},
		{
			desc: "tool_choice.function without name",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{}}}`,
		},
		{
			desc: "tool_calls[].function without name",
			body: `{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"arguments":"{}"}}]}]}`,
		},
		{
			desc: "function_call without name",
			body: `{"model":"gpt-4","messages":[{"role":"assistant","function_call":{"arguments":"{}"}}]}`,
		},
		{
			desc: "top-level functions[] element without name",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"description":"d"}]}`,
		},
		{
			desc: "top-level function_call without name",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"function_call":{}}`,
		},
	}

	for _, tc := range bodies {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out := mustAnonymize(t, f, []byte(tc.body))

			if f.Touched() {
				t.Errorf("Touched() = true, want false: no PII anywhere in body; body: %s", out)
			}
			if string(out) != tc.body {
				t.Errorf("AnonymizeJSON(%s) = %s, want byte-identical to input %s", tc.desc, out, tc.body)
			}
		})
	}
}

// TestFilter_FunctionName_NullName_PriorBehaviorPreserved verifies that a
// JSON null "name" at each location keeps that location's behavior from
// before this shared policy existed: tools[].function.name,
// messages[].tool_calls[].function.name, messages[].function_call.name, and
// the legacy top-level functions[].name were entirely unvalidated before
// (any shape, including null, passed through untouched) and remain untouched
// here; the legacy top-level function_call.name fell into the default
// unknown-field scan before, where a null leaf was left unchanged, and
// remains untouched here too. tool_choice.function.name is the one location
// that already had dedicated validation: a null there unmarshals to an empty
// string, which fails functionNamePattern's 1-character minimum, so it was
// rejected fail-closed before this change and still is.
func TestFilter_FunctionName_NullName_PriorBehaviorPreserved(t *testing.T) {
	t.Parallel()

	untouched := []struct {
		desc string
		body string
	}{
		{
			desc: "tools[].function.name null",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":null,"description":"d"}}]}`,
		},
		{
			desc: "tool_calls[].function.name null",
			body: `{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":null,"arguments":"{}"}}]}]}`,
		},
		{
			desc: "function_call.name null",
			body: `{"model":"gpt-4","messages":[{"role":"assistant","function_call":{"name":null,"arguments":"{}"}}]}`,
		},
		{
			desc: "top-level functions[].name null",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":null,"description":"d"}]}`,
		},
		{
			desc: "top-level function_call.name null",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"function_call":{"name":null}}`,
		},
	}

	for _, tc := range untouched {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out := mustAnonymize(t, f, []byte(tc.body))

			if f.Touched() {
				t.Errorf("Touched() = true, want false: null name is a no-op at this location; body: %s", out)
			}
			if string(out) != tc.body {
				t.Errorf("AnonymizeJSON(%s) = %s, want byte-identical to input %s", tc.desc, out, tc.body)
			}
		})
	}

	t.Run("tool_choice.function.name null fails closed", func(t *testing.T) {
		t.Parallel()

		body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":null}}}`
		f := newTestFilter(t)
		_, err := f.AnonymizeJSON([]byte(body))
		if err == nil {
			t.Fatal("expected fail-closed error for tool_choice.function.name:null, got nil")
		}
		if err.Error() != errAnonymizeMsg {
			t.Errorf("error = %q, want static message %q", err.Error(), errAnonymizeMsg)
		}
	})
}
