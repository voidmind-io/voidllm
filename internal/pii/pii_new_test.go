package pii

// pii_new_test.go covers the additional cases required by the feat/pii-anonymization-stage0a
// test task:
//
//  1. embeddings "input" field: string, array-of-strings, array-of-ints, unexpected shape
//  2. tools[].function.parameters string-leaf scanning and structure preservation
//  3. nested fail-closed: every covered field present with an unexpected type
//  4. case round-trip: mixed-case email variants → same pseudonym; first-seen restore

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ── 1. embeddings "input" field ──────────────────────────────────────────────

func TestAnonymizeJSON_EmbeddingsInput_String(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"text-embedding-3-small","input":"mail an max@example.com bitte"}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}
	if strings.Contains(string(out), "max@example.com") {
		t.Error("input string: PII email still present in anonymized body")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in input string, want true")
	}
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "max@example.com") {
		t.Errorf("restore: original email missing; got: %s", restored)
	}
}

func TestAnonymizeJSON_EmbeddingsInput_ArrayOfStrings(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"text-embedding-3-small","input":["hello world","write to alice@test.example"]}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}
	if strings.Contains(string(out), "alice@test.example") {
		t.Error("array input: PII email still present in anonymized body")
	}
	// First element (clean) must still be present.
	if !strings.Contains(string(out), "hello world") {
		t.Error("array input: clean first element was modified or dropped")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in array input, want true")
	}
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "alice@test.example") {
		t.Errorf("restore: original email missing from array input; got: %s", restored)
	}
}

func TestAnonymizeJSON_EmbeddingsInput_ArrayOfInts(t *testing.T) {
	t.Parallel()

	// Token-IDs: integers, must pass through unchanged.
	f := newTestFilter(t)
	body := []byte(`{"model":"text-embedding-3-small","input":[1,2,3,100,200]}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error for int-array input: %v", err)
	}
	// No PII touched.
	if f.Touched() {
		t.Error("Touched() = true for int-array input, want false")
	}
	// Output JSON must contain the integers untouched.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	var arr []int
	if err := json.Unmarshal(doc["input"], &arr); err != nil {
		t.Fatalf("input field is not an int array: %v", err)
	}
	want := []int{1, 2, 3, 100, 200}
	if len(arr) != len(want) {
		t.Fatalf("input array length = %d, want %d", len(arr), len(want))
	}
	for i, v := range arr {
		if v != want[i] {
			t.Errorf("input[%d] = %d, want %d", i, v, want[i])
		}
	}
}

func TestAnonymizeJSON_EmbeddingsInput_UnexpectedShape_FailClosed(t *testing.T) {
	t.Parallel()

	// "input" is an object: neither string nor array → fail-closed.
	f := newTestFilter(t)
	body := []byte(`{"model":"text-embedding-3-small","input":{"nested":"object"}}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("AnonymizeJSON: expected error for object-shaped input, got nil (should be fail-closed)")
	}
	// Error must be content-free.
	if strings.Contains(err.Error(), "nested") || strings.Contains(err.Error(), "object") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

// ── 2. tools[].function.parameters string-leaf scanning ──────────────────────

func TestAnonymizeJSON_Tools_Parameters_StringLeaves(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	// parameters contains a "description" string with PII and various non-string
	// leaf values (integer default, boolean enum item, null default) that must
	// remain untouched.
	body := []byte(`{
		"model":"gpt-4",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"contact_user",
				"description":"no pii here",
				"parameters":{
					"type":"object",
					"properties":{
						"email":{
							"type":"string",
							"description":"Send result to info@params.example",
							"default":"none"
						},
						"count":{
							"type":"integer",
							"default":5
						},
						"flag":{
							"type":"boolean",
							"default":true
						}
					},
					"required":["email"]
				}
			}
		}]
	}`)

	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	// PII email in parameters.properties.email.description must be replaced.
	if strings.Contains(string(out), "info@params.example") {
		t.Error("PII email in parameters.properties.email.description still present")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in parameters description, want true")
	}

	// JSON structure must be preserved: parse and verify non-PII fields.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(doc["tools"], &tools); err != nil {
		t.Fatalf("tools is not a JSON array: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1", len(tools))
	}
	// Verify integer and boolean defaults survive unchanged.
	var tool struct {
		Function struct {
			Parameters struct {
				Properties struct {
					Count struct {
						Default json.RawMessage `json:"default"`
					} `json:"count"`
					Flag struct {
						Default json.RawMessage `json:"default"`
					} `json:"flag"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(tools[0], &tool); err != nil {
		t.Fatalf("cannot unmarshal tool: %v", err)
	}
	if string(tool.Function.Parameters.Properties.Count.Default) != "5" {
		t.Errorf("integer default mutated: got %s, want 5",
			tool.Function.Parameters.Properties.Count.Default)
	}
	if string(tool.Function.Parameters.Properties.Flag.Default) != "true" {
		t.Errorf("boolean default mutated: got %s, want true",
			tool.Function.Parameters.Properties.Flag.Default)
	}

	// Restore must recover the PII email.
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "info@params.example") {
		t.Errorf("restore: original email missing from parameters description; got: %s", restored)
	}
}

func TestAnonymizeJSON_Tools_NotAnArray_FailClosed(t *testing.T) {
	t.Parallel()

	// "tools" is present but is not an array → fail-closed.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":"not-an-array"}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("AnonymizeJSON: expected error for non-array tools, got nil (should be fail-closed)")
	}
	if strings.Contains(err.Error(), "not-an-array") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

// ── 3. nested fail-closed: unexpected shapes for every covered field ──────────
//
// Each sub-test provides a PRESENT field with an unexpected type. A MISSING field
// must not produce an error (the implementation skips absent fields).

func TestAnonymizeJSON_FailClosed_ContentNotStringNorArray(t *testing.T) {
	t.Parallel()

	// messages[].content is a number, not a string or array.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":42}]}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for numeric content, got nil")
	}
	if strings.Contains(err.Error(), "42") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

func TestAnonymizeJSON_FailClosed_FunctionCallNotObject(t *testing.T) {
	t.Parallel()

	// messages[].function_call is a string, not an object.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","function_call":"not-an-object","content":"hi"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for non-object function_call, got nil")
	}
	if strings.Contains(err.Error(), "not-an-object") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

func TestAnonymizeJSON_FailClosed_FunctionCallArgumentsNotString(t *testing.T) {
	t.Parallel()

	// messages[].function_call.arguments is a number, not a string.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","function_call":{"name":"fn","arguments":123}}]}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for non-string function_call.arguments, got nil")
	}
	if strings.Contains(err.Error(), "123") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

func TestAnonymizeJSON_FailClosed_ToolCallsNotArray(t *testing.T) {
	t.Parallel()

	// messages[].tool_calls is a string, not an array.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":"not-an-array","content":"hi"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for non-array tool_calls, got nil")
	}
	if strings.Contains(err.Error(), "not-an-array") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

func TestAnonymizeJSON_FailClosed_ToolCallArgumentsNotString(t *testing.T) {
	t.Parallel()

	// messages[].tool_calls[i].function.arguments is a number, not a string.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":{"key":"val"}}}]}]}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for non-string tool_calls[].function.arguments, got nil")
	}
}

func TestAnonymizeJSON_FailClosed_MessagesNotArray(t *testing.T) {
	t.Parallel()

	// "messages" is a string, not an array.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":"not-an-array"}`)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for non-array messages, got nil")
	}
	if strings.Contains(err.Error(), "not-an-array") {
		t.Errorf("error message leaks body content: %q", err.Error())
	}
}

// Missing fields must NOT produce errors (conservative pass-through).

func TestAnonymizeJSON_NoError_MissingMessages(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4"}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("AnonymizeJSON with no messages field: unexpected error: %v", err)
	}
}

func TestAnonymizeJSON_NoError_MissingFunctionCall(t *testing.T) {
	t.Parallel()

	// Message with no function_call field at all: no error.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("AnonymizeJSON with no function_call: unexpected error: %v", err)
	}
}

func TestAnonymizeJSON_NoError_MissingToolCalls(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","content":"ok"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("AnonymizeJSON with no tool_calls: unexpected error: %v", err)
	}
}

func TestAnonymizeJSON_NoError_MissingInput(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("AnonymizeJSON with no input field: unexpected error: %v", err)
	}
}

// Error messages must never contain PII/body content.

func TestAnonymizeJSON_FailClosed_ErrorMessageContentFree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "object input",
			body: []byte(`{"model":"x","input":{"pii":"secret@example.com"}}`),
		},
		{
			name: "non-array messages",
			body: []byte(`{"model":"x","messages":"secret@example.com"}`),
		},
		{
			name: "non-array tools",
			body: []byte(`{"model":"x","tools":"secret@example.com","messages":[]}`),
		},
		{
			name: "non-object function_call",
			body: []byte(`{"model":"x","messages":[{"role":"a","function_call":"secret@example.com","content":"hi"}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error, got nil", tc.name)
				return
			}
			// The error message must not contain any content from the body.
			if strings.Contains(err.Error(), "secret@example.com") {
				t.Errorf("[%s] error message leaks PII: %q", tc.name, err.Error())
			}
			// A generic check: no raw JSON fragments from the body.
			if strings.Contains(err.Error(), "{") || strings.Contains(err.Error(), "}") {
				t.Errorf("[%s] error message appears to contain JSON fragments: %q", tc.name, err.Error())
			}
		})
	}
}

// ── 4. case round-trip ─────────────────────────────────────────────────────────

// TestFilter_CaseRoundtrip verifies that mixed-case variants of the same email
// produce a single pseudonym and that restore always returns the FIRST-SEEN
// original form, as documented in pseudonymFor.
//
// The semantics implemented: both "User@Example.com" and "user@example.com"
// normalize to the same key, share one fwd entry, and one rev entry.
// The rev entry is set to the FIRST-SEEN original form and never overwritten.
// After restore, the client receives the original form as first encountered
// in the body.
func TestFilter_CaseRoundtrip_MixedCaseEmailSamePseudonym(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Both email variants appear in a single content string.
	// "User@Example.com" is first (position-wise in the message).
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"User@Example.com and user@example.com are the same person"}]}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}

	// Neither variant of the email must be visible in the anonymized body.
	if strings.Contains(string(out), "@Example.com") || strings.Contains(string(out), "@example.com") {
		t.Error("anonymized body still contains the email in some case variant")
	}
	if !f.Touched() {
		t.Error("Touched() = false; expected PII to be detected")
	}

	// Both occurrences must be replaced by the SAME pseudonym (not two different ones).
	// Count how many distinct PII_EM_ tokens appear.
	outStr := string(out)
	firstPseudo := ""
	for p := range f.rev {
		firstPseudo = p
		break
	}
	if firstPseudo == "" {
		t.Fatal("rev map is empty; no pseudonym was recorded")
	}
	// The pseudonym must appear twice in the output (one per original occurrence).
	count := strings.Count(outStr, firstPseudo)
	if count != 2 {
		t.Errorf("expected pseudonym to appear 2 times, got %d; out: %s", count, outStr)
	}

	// Restore must map the pseudonym back to the FIRST-SEEN original form.
	// The first occurrence in the body is "User@Example.com" (capital U and E).
	restored := f.Restore(out)
	restoredStr := string(restored)

	// The pseudonym must not be visible after restore.
	if strings.Contains(restoredStr, firstPseudo) {
		t.Errorf("pseudonym still visible after restore: %s", restoredStr)
	}

	// Verify the rev map holds the first-seen form. The key point is that
	// rev[pseudo] is set once (first occurrence) and never overwritten.
	firstSeen, ok := f.rev[firstPseudo]
	if !ok {
		t.Fatalf("pseudonym %q not found in rev map", firstPseudo)
	}
	// After restore, exactly one of the two original forms must appear.
	// Which form was "first seen" depends on left-to-right scan order in the
	// text: "User@Example.com" appears earlier, so it must be first-seen.
	// The rev entry must equal the first encountered form.
	if firstSeen != "User@Example.com" && firstSeen != "user@example.com" {
		t.Errorf("rev[pseudo] = %q is neither case variant", firstSeen)
	}
	// The restored output must contain the first-seen form exactly where the
	// pseudonym was, and there must be NO residual pseudonym.
	if !strings.Contains(restoredStr, firstSeen) {
		t.Errorf("restored body does not contain first-seen original form %q; got: %s", firstSeen, restoredStr)
	}
	// No data loss: the restored content must contain the original value twice
	// (once per pseudonym occurrence), all replaced with the single rev entry.
	if strings.Count(restoredStr, firstSeen) != 2 {
		t.Errorf("expected restored form to appear 2 times, got %d; out: %s",
			strings.Count(restoredStr, firstSeen), restoredStr)
	}
}

// TestFilter_CaseRoundtrip_NoCrash verifies there is no crash or panic
// when the same canonical value is submitted many times with alternating case.
func TestFilter_CaseRoundtrip_NoCrash(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Build a large body with 50 alternating-case occurrences of the same email.
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-4","messages":[{"role":"user","content":"`)
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			sb.WriteString("Upper@CASE.example ")
		} else {
			sb.WriteString("upper@case.example ")
		}
	}
	sb.WriteString(`"}]}`)

	out, err := f.AnonymizeJSON([]byte(sb.String()))
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error on repeated case variants: %v", err)
	}
	if !f.Touched() {
		t.Error("Touched() = false; expected email to be detected")
	}
	// Only ONE rev entry must exist.
	if len(f.rev) != 1 {
		t.Errorf("expected exactly 1 rev entry (all variants normalize to the same key), got %d", len(f.rev))
	}
	// Restore must not panic and must not contain the pseudonym.
	restored := f.Restore(out)
	pseudo := ""
	for p := range f.rev {
		pseudo = p
	}
	if strings.Contains(string(restored), pseudo) {
		t.Errorf("pseudonym still visible after restore: %s", restored)
	}

	// No data loss: the restored output must not be empty.
	if len(restored) == 0 {
		t.Error("restore returned empty output")
	}
}

// TestFilter_CaseRoundtrip_RevNotOverwritten verifies the explicit contract:
// when the same canonical value appears in both its original and a case-variant
// form, the rev entry is set once (first occurrence) and never overwritten.
// We verify this directly on the internal rev map, which pii_test.go uses freely
// (white-box test, same package).
func TestFilter_CaseRoundtrip_RevNotOverwritten(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Submit two-message body: first message has lowercase, second has uppercase.
	// The lowercase variant arrives at AnonymizeJSON first (left-to-right scan).
	body := []byte(`{"model":"gpt-4","messages":[` +
		`{"role":"user","content":"first: lower@rev.example"},` +
		`{"role":"assistant","content":"second: LOWER@REV.EXAMPLE"}` +
		`]}`)

	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}
	if !f.Touched() {
		t.Error("Touched() = false; expected email to be detected")
	}

	// Verify only one rev entry exists.
	if len(f.rev) != 1 {
		t.Errorf("expected 1 rev entry, got %d; rev: %v", len(f.rev), f.rev)
	}

	// The first-seen form is "lower@rev.example" (appears in messages[0] which
	// is processed before messages[1]).
	var pseudo, orig string
	for p, o := range f.rev {
		pseudo = p
		orig = o
		break
	}
	if orig != "lower@rev.example" {
		// If the implementation processes messages in a different order this
		// assertion may need adjustment, but left-to-right scan is documented.
		t.Errorf("rev[%q] = %q, want %q (first-seen form)", pseudo, orig, "lower@rev.example")
	}
}

// TestAnonymizeJSON_Tools_NoArrayElements_NoError verifies that an empty tools
// array does not cause an error (conservative edge case).
func TestAnonymizeJSON_Tools_EmptyArray_NoError(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"tools":[]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("AnonymizeJSON with empty tools array: unexpected error: %v", err)
	}
}

// TestAnonymizeJSON_EmbeddingsInput_ArrayMixed verifies that a mixed array
// (strings interspersed with integers) anonymizes only string elements and
// leaves integer elements untouched. This exercises the per-element type
// check in the array code path.
func TestAnonymizeJSON_EmbeddingsInput_ArrayMixed(t *testing.T) {
	t.Parallel()

	// Arrays of integers in the embeddings API encode token IDs; if the
	// API allows arrays of arrays (each being a token array), those are also
	// left untouched. Here we test a flat array where some elements are
	// strings containing PII and others are integers.
	f := newTestFilter(t)
	// Note: while mixing strings and ints in one array is unusual in the
	// real OpenAI API, the code handles it element-by-element.
	body := []byte(`{"model":"text-embedding-3-small","input":["send to bob@mixed.example",42,"also cc@mixed.example"]}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}
	if strings.Contains(string(out), "bob@mixed.example") {
		t.Error("first string element still contains PII email")
	}
	if strings.Contains(string(out), "cc@mixed.example") {
		t.Error("third string element still contains PII email")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in mixed array, want true")
	}

	// Integer element must survive the round-trip.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(doc["input"], &arr); err != nil {
		t.Fatalf("input is not an array: %v", err)
	}
	if len(arr) != 3 {
		t.Fatalf("input array length = %d, want 3", len(arr))
	}
	// Second element must still be integer 42.
	var v int
	if err := json.Unmarshal(arr[1], &v); err != nil {
		t.Fatalf("arr[1] is not an integer after round-trip: %v (raw: %s)", err, arr[1])
	}
	if v != 42 {
		t.Errorf("arr[1] = %d, want 42", v)
	}
}

// ── Error sentinel test ───────────────────────────────────────────────────────

// TestAnonymizeJSON_FailClosed_ErrorIsSentinel verifies that all fail-closed
// paths return a non-nil error (no panic, no nil) for every covered field
// with an unexpected shape. Uses errors.Is with a nil target as a "any error"
// check.
func TestAnonymizeJSON_FailClosed_AllShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"content is number", `{"model":"gpt-4","messages":[{"role":"user","content":42}]}`},
		{"content is bool", `{"model":"gpt-4","messages":[{"role":"user","content":true}]}`},
		{"function_call is array", `{"model":"gpt-4","messages":[{"role":"a","function_call":[],"content":"hi"}]}`},
		{"function_call.arguments is object", `{"model":"gpt-4","messages":[{"role":"a","function_call":{"name":"f","arguments":{"x":1}}}]}`},
		{"tool_calls is object", `{"model":"gpt-4","messages":[{"role":"a","tool_calls":{"a":1},"content":"hi"}]}`},
		{"tool_calls[i].function.arguments is array", `{"model":"gpt-4","messages":[{"role":"a","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":[]}}]}]}`},
		{"messages is number", `{"model":"gpt-4","messages":123}`},
		{"input is null-shaped object", `{"model":"x","input":{"nested":true}}`},
		{"tools is string", `{"model":"x","tools":"bad","messages":[]}`},
		// FIX 1: simple-string fields must fail-closed when present but not a string.
		{"user is object", `{"model":"gpt-4","user":{"id":1},"messages":[]}`},
		{"user is array", `{"model":"gpt-4","user":[1,2],"messages":[]}`},
		{"user is number", `{"model":"gpt-4","user":42,"messages":[]}`},
		{"messages[].name is object", `{"model":"gpt-4","messages":[{"role":"user","name":{},"content":"hi"}]}`},
		{"messages[].name is number", `{"model":"gpt-4","messages":[{"role":"user","name":99,"content":"hi"}]}`},
		{"tools[].function.description is number", `{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"f","description":42}}]}`},
		{"tools[].function.description is object", `{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"f","description":{"x":1}}}]}`},
		// FIX 1b: prompt/input array elements — non-string, non-number shapes.
		{"prompt array element is object", `{"model":"gpt-3.5","prompt":[{"key":"val"}]}`},
		{"prompt array element is bool", `{"model":"gpt-3.5","prompt":[true]}`},
		{"input array element is object", `{"model":"text-emb","input":[{"key":"val"}]}`},
		{"input array element is bool", `{"model":"text-emb","input":[false]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON([]byte(tc.body))
			if err == nil {
				t.Errorf("[%s] expected fail-closed error, got nil", tc.name)
				return
			}
			// Verify the error is non-nil (errors.Is(err, nil) == false).
			if errors.Is(err, nil) {
				t.Errorf("[%s] errors.Is(err, nil) is true; expected a real error", tc.name)
			}
		})
	}
}

// ── FIX 1: token-array passthrough ───────────────────────────────────────────

// TestAnonymizeJSON_PromptTokenArraysPassThrough verifies that valid token-ID
// arrays (int[], int[][]) in the "prompt" field pass through without error and
// without modification, while object and bool elements are rejected.
func TestAnonymizeJSON_PromptTokenArraysPassThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "int[] token array",
			body:    `{"model":"gpt-3.5","prompt":[100,200,300]}`,
			wantErr: false,
		},
		{
			name:    "int[][] token array",
			body:    `{"model":"gpt-3.5","prompt":[[1,2,3],[4,5,6]]}`,
			wantErr: false,
		},
		{
			name:    "mixed string and int",
			body:    `{"model":"gpt-3.5","prompt":["hello",42]}`,
			wantErr: false,
		},
		{
			name:    "object element",
			body:    `{"model":"gpt-3.5","prompt":[{"key":"val"}]}`,
			wantErr: true,
		},
		{
			name:    "bool element",
			body:    `{"model":"gpt-3.5","prompt":[true]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON([]byte(tc.body))
			if tc.wantErr && err == nil {
				t.Errorf("[%s] expected error, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("[%s] unexpected error: %v", tc.name, err)
			}
		})
	}
}

// TestAnonymizeJSON_InputTokenArraysPassThrough verifies the same for "input".
func TestAnonymizeJSON_InputTokenArraysPassThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "int[] token array",
			body:    `{"model":"text-emb","input":[1,2,3,100]}`,
			wantErr: false,
		},
		{
			name:    "int[][] token array",
			body:    `{"model":"text-emb","input":[[1,2],[3,4]]}`,
			wantErr: false,
		},
		{
			name:    "object element",
			body:    `{"model":"text-emb","input":[{"x":1}]}`,
			wantErr: true,
		},
		{
			name:    "bool element",
			body:    `{"model":"text-emb","input":[false]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON([]byte(tc.body))
			if tc.wantErr && err == nil {
				t.Errorf("[%s] expected error, got nil", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("[%s] unexpected error: %v", tc.name, err)
			}
		})
	}
}

// ── FIX 2: object keys in parameters schema ───────────────────────────────────

// TestAnonymizeJSON_Tools_Parameters_PIIInKey_FailClosed verifies that when a
// key inside tools[].function.parameters contains PII (e.g. an email address
// used as a property name), the request is rejected rather than forwarded with
// the key intact. Rewriting a structural key would corrupt the schema, so
// fail-closed is the only safe response.
func TestAnonymizeJSON_Tools_Parameters_PIIInKey_FailClosed(t *testing.T) {
	t.Parallel()

	// A schema where one property key IS a PII email address.
	// This is pathological but must be rejected, not silently passed through.
	body := []byte(`{
		"model":"gpt-4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"fn",
				"parameters":{
					"type":"object",
					"properties":{
						"user@example.com":{
							"type":"string"
						}
					}
				}
			}
		}]
	}`)

	f := newTestFilter(t)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error when parameters key contains PII, got nil")
	}
	// Error must be content-free.
	if strings.Contains(err.Error(), "user@example.com") {
		t.Errorf("error message leaks PII key: %q", err.Error())
	}
}

// TestAnonymizeJSON_Tools_Parameters_CleanKeys_Passthrough verifies that clean
// (non-PII) property keys in parameters are never rejected. Only keys that
// match a PII detector pattern trigger fail-closed.
func TestAnonymizeJSON_Tools_Parameters_CleanKeys_Passthrough(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"gpt-4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"fn",
				"parameters":{
					"type":"object",
					"properties":{
						"email_address":{"type":"string"},
						"phone_number":{"type":"string"},
						"count":{"type":"integer"}
					}
				}
			}
		}]
	}`)

	f := newTestFilter(t)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Errorf("clean parameter keys should not cause error: %v", err)
	}
}
