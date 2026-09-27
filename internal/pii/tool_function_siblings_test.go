package pii

// tool_function_siblings_test.go covers the fix that makes the dedicated
// tools[] and functions[] handlers in json.go scan every key other than the
// ones they handle explicitly (name, description, parameters, and for
// tools[] the type/function wrapper) with scanStringLeaves, instead of
// silently forwarding unknown sibling keys unscanned. It follows the
// conventions in pii_test.go and unknown_fields_test.go: newTestFilter/
// mustAnonymize helpers and white-box package pii.

import (
	"strings"
	"testing"
)

// TestFilter_Functions_MetadataSibling_Pseudonymized verifies that a
// functions[] element carrying an unrecognized sibling key (e.g. a
// provider-specific "metadata" object) alongside "name"/"description" is
// scanned by the same string-leaf scan used for "parameters" — regressing to
// forwarding it unscanned would leak an email address nested inside it.
func TestFilter_Functions_MetadataSibling_Pseudonymized(t *testing.T) {
	t.Parallel()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"get_weather","description":"looks up the weather","metadata":{"owner":"alice@example.com"}}]}`

	f := newTestFilter(t)
	out := mustAnonymize(t, f, []byte(body))

	if !f.Touched() {
		t.Fatalf("Touched() = false, want true: functions[].metadata.owner carries an email; body: %s", out)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Errorf("anonymized body still contains original email: %s", out)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("anonymized body lost the untouched function name: %s", out)
	}
	if !strings.Contains(string(out), `"description":"looks up the weather"`) {
		t.Errorf("anonymized body lost the untouched description: %s", out)
	}

	restored := f.Restore(out)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restored body missing original email: %s", restored)
	}
}

// TestFilter_Tools_ToolLevelSibling_Pseudonymized verifies that a tools[]
// element carrying an unrecognized sibling key at the tool level (a sibling
// of "type"/"function", e.g. "metadata" or a non-function tool type's own
// payload) is scanned, not forwarded unscanned.
func TestFilter_Tools_ToolLevelSibling_Pseudonymized(t *testing.T) {
	t.Parallel()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"looks up the weather"},"metadata":{"owner":"alice@example.com"}}]}`

	f := newTestFilter(t)
	out := mustAnonymize(t, f, []byte(body))

	if !f.Touched() {
		t.Fatalf("Touched() = false, want true: tools[].metadata.owner carries an email; body: %s", out)
	}
	if strings.Contains(string(out), "alice@example.com") {
		t.Errorf("anonymized body still contains original email: %s", out)
	}
	if !strings.Contains(string(out), `"type":"function"`) {
		t.Errorf("anonymized body lost the untouched type discriminator: %s", out)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("anonymized body lost the untouched function name: %s", out)
	}

	restored := f.Restore(out)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restored body missing original email: %s", restored)
	}
}

// TestFilter_Tools_FunctionLevelSibling_Pseudonymized verifies that
// tools[].function carrying an unrecognized sibling key alongside
// "name"/"description"/"parameters" (e.g. a "strict"-adjacent free-text field
// some clients attach) is scanned, not forwarded unscanned.
func TestFilter_Tools_FunctionLevelSibling_Pseudonymized(t *testing.T) {
	t.Parallel()

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"looks up the weather","custom":"contact bob@example.com"}}]}`

	f := newTestFilter(t)
	out := mustAnonymize(t, f, []byte(body))

	if !f.Touched() {
		t.Fatalf("Touched() = false, want true: tools[].function.custom carries an email; body: %s", out)
	}
	if strings.Contains(string(out), "bob@example.com") {
		t.Errorf("anonymized body still contains original email: %s", out)
	}
	if !strings.Contains(string(out), `"name":"get_weather"`) {
		t.Errorf("anonymized body lost the untouched function name: %s", out)
	}

	restored := f.Restore(out)
	if !strings.Contains(string(restored), "bob@example.com") {
		t.Errorf("restored body missing original email: %s", restored)
	}
}

// TestFilter_Functions_Tools_NoPII_ByteIdentical verifies that
// functions[]/tools[] bodies without any PII in their sibling keys — or
// without any sibling keys at all — are returned completely byte-identical,
// so the fix does not force a re-marshal (and the ordering/formatting churn
// that comes with it) on the common case.
func TestFilter_Functions_Tools_NoPII_ByteIdentical(t *testing.T) {
	t.Parallel()

	bodies := []struct {
		desc string
		body string
	}{
		{
			desc: "functions[] with non-PII sibling",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"get_weather","description":"looks up the weather","metadata":{"owner":"internal-service"}}]}`,
		},
		{
			desc: "functions[] with no sibling keys",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"get_weather","description":"looks up the weather"}]}`,
		},
		{
			desc: "tools[] with non-PII tool-level and function-level siblings",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"looks up the weather","custom":"no pii here"},"metadata":{"owner":"internal-service"}}]}`,
		},
		{
			desc: "tools[] with no sibling keys",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"looks up the weather"}}]}`,
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
