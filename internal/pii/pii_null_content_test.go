package pii

// pii_null_content_test.go covers the fix/pii-null-content branch:
// messages[].content == JSON null is now skipped (no scan, no 422) while
// other fields in the same message (tool_calls, name, etc.) are still
// scanned. Unsupported non-null scalar shapes (number, bool, object) still
// trigger fail-closed exactly as before.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// piiTokenRE matches any pseudonym token produced by the filter under the
// DefaultPatterns detector set. The format is PII_<2-char abbr>_<24 hex chars>.
var piiTokenRE = regexp.MustCompile(`PII_[A-Z]{2}_[0-9a-f]{24}`)

// ── Test 1: assistant message with content:null + tool_calls containing IBAN ──

// TestAnonymizeJSON_NullContent_ToolCallsPseudonymized verifies that an
// assistant message whose "content" is JSON null is not rejected (no error),
// that "touched" is set to true because the IBAN in tool_calls[].function.arguments
// is pseudonymized, and that the raw IBAN no longer appears in the output.
func TestAnonymizeJSON_NullContent_ToolCallsPseudonymized(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Single assistant message: content is null, tool_calls carries an IBAN.
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"iban\":\"DE89370400440532013000\"}"}}` +
		`]}` +
		`]}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error (content:null must not 422): %v", err)
	}

	// The raw IBAN must not be present in the output.
	if strings.Contains(string(out), "DE89370400440532013000") {
		t.Error("output still contains raw IBAN; pseudonymization of tool_calls failed")
	}

	// At least one PII token must appear in the output.
	tokens := piiTokenRE.FindAllString(string(out), -1)
	if len(tokens) == 0 {
		t.Errorf("no PII_ token found in output; expected IBAN to be pseudonymized: %s", out)
	}

	// Touched must be true because we replaced PII in tool_calls.
	if !f.Touched() {
		t.Error("Touched() = false; expected true because IBAN in tool_calls was pseudonymized")
	}

	// Restore must put the original IBAN back.
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "DE89370400440532013000") {
		t.Errorf("Restore did not recover the original IBAN; got: %s", restored)
	}
}

// ── Test 2: full multi-turn tool-use conversation ───────────────────────────

// TestAnonymizeJSON_NullContent_MultiTurnToolHistory verifies that a realistic
// multi-turn tool-calling conversation is handled correctly:
//
//  1. User message with text PII (email) — must be pseudonymized.
//  2. Assistant message with content:null + tool_calls — no error, IBAN in
//     arguments must be pseudonymized.
//  3. Tool-result message (role:"tool") with email in content — must be
//     pseudonymized.
func TestAnonymizeJSON_NullContent_MultiTurnToolHistory(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[` +
		// Turn 1: user sends an email address.
		`{"role":"user","content":"Please contact user@example.com"},` +
		// Turn 2: assistant responds with content:null and a tool call carrying an IBAN.
		`{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"transfer","arguments":"{\"iban\":\"DE89370400440532013000\"}"}}` +
		`]},` +
		// Turn 3: tool result containing an email.
		`{"role":"tool","tool_call_id":"call_1","content":"Sent to user@example.com"}` +
		`]}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	outStr := string(out)

	// Email in user message must be gone.
	if strings.Contains(outStr, "user@example.com") {
		t.Error("user message still contains original email address")
	}

	// IBAN in tool_calls must be gone.
	if strings.Contains(outStr, "DE89370400440532013000") {
		t.Error("tool_calls arguments still contain the raw IBAN")
	}

	// Email in tool result must be gone.
	// (Both occurrences share the same pseudonym because they are the same value.)
	if strings.Contains(outStr, "user@example.com") {
		t.Error("tool result still contains original email address")
	}

	// At least two distinct PII tokens must appear (one EMAIL, one IBAN).
	tokens := piiTokenRE.FindAllString(outStr, -1)
	if len(tokens) < 2 {
		t.Errorf("expected at least 2 PII token occurrences, got %d; output: %s", len(tokens), outStr)
	}

	if !f.Touched() {
		t.Error("Touched() = false; expected true because multiple PII values were replaced")
	}

	// Restore must recover both PII values.
	restored := string(f.Restore(out))
	if !strings.Contains(restored, "user@example.com") {
		t.Errorf("Restore did not recover original email; got: %s", restored)
	}
	if !strings.Contains(restored, "DE89370400440532013000") {
		t.Errorf("Restore did not recover original IBAN; got: %s", restored)
	}
}

// ── Test 3: assistant message with content:null and no other fields ──────────

// TestAnonymizeJSON_NullContent_NoOtherFields verifies that an assistant message
// with only "role" and "content":null produces no error, is returned with
// Touched == false, and that the output body is semantically identical to the
// input (no spurious modification).
func TestAnonymizeJSON_NullContent_NoOtherFields(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","content":null}]}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error for content:null with no other fields: %v", err)
	}

	if f.Touched() {
		t.Error("Touched() = true; expected false because there is no scannable content")
	}

	// The output must be valid JSON and the content field must still be null.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v; got: %s", err, out)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(doc["messages"], &messages); err != nil {
		t.Fatalf("messages is not a JSON array: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages length = %d, want 1", len(messages))
	}
	if string(messages[0]["content"]) != "null" {
		t.Errorf("content field was modified; got %s, want null", messages[0]["content"])
	}
}

// ── Test 4: regression — unsupported non-null scalar content shapes still fail ──

// TestAnonymizeJSON_NullContent_FailClosed_UnsupportedScalars verifies that
// content values that are numbers, booleans, or objects continue to trigger
// fail-closed, i.e. the null-guard does not accidentally let bad shapes through.
func TestAnonymizeJSON_NullContent_FailClosed_UnsupportedScalars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "content is number",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":42}]}`),
		},
		{
			name: "content is boolean true",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":true}]}`),
		},
		{
			name: "content is boolean false",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":false}]}`),
		},
		{
			name: "content is object",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":{"key":"value"}}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for unsupported content shape, got nil", tc.name)
				return
			}
			// Error message must be content-free.
			for _, leak := range []string{"42", "true", "false", "key", "value"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("[%s] error message leaks body content %q: %s", tc.name, leak, err.Error())
				}
			}
		})
	}
}

// ── Test 5: no regression for string content and array-of-parts content ──────

// TestAnonymizeJSON_NullContent_NoRegress_StringContent verifies that the
// null-guard does not break normal string content pseudonymization.
func TestAnonymizeJSON_NullContent_NoRegress_StringContent(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"contact me at alice@example.com please"}]}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	if strings.Contains(string(out), "alice@example.com") {
		t.Error("string content: email still present after anonymization")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in string content; expected true")
	}
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("Restore did not recover email from string content; got: %s", restored)
	}
}

// TestAnonymizeJSON_NullContent_NoRegress_ArrayContent verifies that the
// null-guard does not break multi-modal array-of-parts content pseudonymization.
func TestAnonymizeJSON_NullContent_NoRegress_ArrayContent(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"My IBAN is DE12345678901234567890"},` +
		`{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}` +
		`]}]}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	if strings.Contains(string(out), "DE12345678901234567890") {
		t.Error("array content: IBAN still present after anonymization")
	}
	if !strings.Contains(string(out), "https://example.com/img.png") {
		t.Error("array content: image_url was removed or modified")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in array content text part; expected true")
	}
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "DE12345678901234567890") {
		t.Errorf("Restore did not recover IBAN from array content; got: %s", restored)
	}
}

// TestAnonymizeJSON_NullContent_MixedMessages verifies the full mixed-message
// scenario: a regular user message with PII immediately followed by an
// assistant message with content:null (no other fields), ensuring that:
//
//   - no error is returned
//   - PII in the user message is still anonymized
//   - the null content of the assistant message is not modified
//   - Touched is true (because of the user message PII)
func TestAnonymizeJSON_NullContent_MixedMessages(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"user","content":"reach bob@example.com urgently"},` +
		`{"role":"assistant","content":null}` +
		`]}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	outStr := string(out)

	if strings.Contains(outStr, "bob@example.com") {
		t.Error("user message email still present after anonymization")
	}

	// Parse the output and verify the assistant message content is still null.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v; got: %s", err, out)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(doc["messages"], &messages); err != nil {
		t.Fatalf("messages is not a JSON array: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2", len(messages))
	}
	if string(messages[1]["content"]) != "null" {
		t.Errorf("assistant message content was modified; got %s, want null", messages[1]["content"])
	}

	if !f.Touched() {
		t.Error("Touched() = false; expected true because user message PII was replaced")
	}
}
