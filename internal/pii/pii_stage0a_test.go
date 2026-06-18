package pii

// pii_stage0a_test.go covers the hardened paths introduced in
// feat/pii-anonymization-stage0a:
//
//  1. Duplicate-key fail-closed (#3): top-level, nested, and deep duplicate keys.
//  2. Token-element validation (#6): float rejection, pathological depth, valid int/int[][].
//  3. Malformed array-element fail-closed (#2): messages/tools/tool_calls/content-parts.
//
// All tests use synthetic, non-real PII.  The package-level test helpers
// (newTestFilter, newTestEngine, etc.) are defined in pii_test.go (same package).

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// ── 1. Duplicate-key fail-closed (#3) ─────────────────────────────────────────

// TestHasDuplicateKeys_TopLevel verifies that a top-level duplicate key is
// detected and returned as (true, nil).
func TestHasDuplicateKeys_TopLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		wantDup bool
	}{
		{
			name:    "top-level duplicate messages key",
			body:    []byte(`{"messages":[{"role":"user","content":"a"}],"messages":[{"role":"user","content":"b"}]}`),
			wantDup: true,
		},
		{
			name:    "top-level duplicate user key",
			body:    []byte(`{"user":"alice","user":"bob"}`),
			wantDup: true,
		},
		{
			name:    "top-level no duplicate",
			body:    []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`),
			wantDup: false,
		},
		{
			name:    "empty object",
			body:    []byte(`{}`),
			wantDup: false,
		},
		{
			name:    "single key no duplicate",
			body:    []byte(`{"model":"gpt-4"}`),
			wantDup: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := hasDuplicateKeys(tc.body)
			if err != nil {
				t.Fatalf("hasDuplicateKeys(%q): unexpected error: %v", tc.body, err)
			}
			if got != tc.wantDup {
				t.Errorf("hasDuplicateKeys(%q) = %v, want %v", tc.body, got, tc.wantDup)
			}
		})
	}
}

// TestHasDuplicateKeys_Nested verifies that duplicate keys nested inside
// array elements or object values are detected.
func TestHasDuplicateKeys_Nested(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		wantDup bool
	}{
		{
			name:    "duplicate in message object",
			body:    []byte(`{"messages":[{"role":"user","content":"x","content":"y"}]}`),
			wantDup: true,
		},
		{
			name:    "duplicate deep in tools.function.parameters",
			body:    []byte(`{"tools":[{"type":"function","function":{"name":"fn","parameters":{"type":"object","type":"array"}}}]}`),
			wantDup: true,
		},
		{
			name:    "clean body with nesting",
			body:    []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[]}`),
			wantDup: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := hasDuplicateKeys(tc.body)
			if err != nil {
				t.Fatalf("hasDuplicateKeys: unexpected error: %v", err)
			}
			if got != tc.wantDup {
				t.Errorf("hasDuplicateKeys = %v, want %v; body: %s", got, tc.wantDup, tc.body)
			}
		})
	}
}

// TestAnonymizeJSON_DuplicateKey_FailClosed verifies that AnonymizeJSON
// rejects bodies with duplicate keys at any nesting level. The error must
// be non-nil and must not contain body content.
func TestAnonymizeJSON_DuplicateKey_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "top-level duplicate messages key",
			body: []byte(`{"messages":[{"role":"user","content":"a"}],"messages":[{"role":"user","content":"b"}]}`),
		},
		{
			name: "top-level duplicate user key",
			body: []byte(`{"user":"alice","user":"bob"}`),
		},
		{
			name: "nested duplicate in message element",
			body: []byte(`{"messages":[{"role":"user","content":"x","content":"y"}]}`),
		},
		{
			name: "deep duplicate in tools parameters",
			body: []byte(`{"tools":[{"type":"function","function":{"name":"fn","parameters":{"type":"object","type":"array"}}}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for duplicate key, got nil", tc.name)
				return
			}
			// Error message must be content-free: must not contain any of the
			// string values from the body.
			for _, leak := range []string{"alice", "bob", "messages", "content", "object", "array"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("[%s] error message leaks body content %q: %s", tc.name, leak, err.Error())
				}
			}
		})
	}
}

// TestAnonymizeJSON_NoDuplicateKey_Passthrough verifies that a clean body
// (no duplicate keys, no PII) passes through without error and is returned
// byte-for-byte identical to the original.
func TestAnonymizeJSON_NoDuplicateKey_Passthrough(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello world"}]}`)
	f := newTestFilter(t)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(clean body): unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("AnonymizeJSON(clean body): body mutated:\n got: %s\nwant: %s", out, body)
	}
	if f.Touched() {
		t.Error("Touched() = true for clean body with no PII, want false")
	}
}

// ── 2. Token-element validation (#6) ──────────────────────────────────────────

// TestIsTokenElement_FloatRejected verifies that float-shaped JSON values are
// not considered valid token elements and cause fail-closed behavior.
func TestIsTokenElement_FloatRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "simple float 1.5", raw: "1.5"},
		{name: "zero-decimal 1.0", raw: "1.0"},
		{name: "negative float", raw: "-3.14"},
		{name: "scientific notation", raw: "1e5"},
		{name: "scientific notation uppercase", raw: "2E3"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := isTokenElement(jsonx.RawMessage(tc.raw), 0)
			if got {
				t.Errorf("isTokenElement(%q) = true, want false (floats must be rejected)", tc.raw)
			}
		})
	}
}

// TestAnonymizeJSON_FloatInPromptArray_FailClosed verifies that a float element
// in the "prompt" array causes AnonymizeJSON to fail-closed.
func TestAnonymizeJSON_FloatInPromptArray_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "float 1.5 in prompt array",
			body: []byte(`{"model":"gpt-3.5","prompt":[1.5]}`),
		},
		{
			name: "float 1.0 in prompt array",
			body: []byte(`{"model":"gpt-3.5","prompt":[1.0]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for float in prompt array, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_FloatInInputArray_FailClosed verifies that a float element
// in the "input" array causes AnonymizeJSON to fail-closed.
func TestAnonymizeJSON_FloatInInputArray_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "float 1.5 in input array",
			body: []byte(`{"model":"text-emb","input":[1.5]}`),
		},
		{
			name: "float 1.0 in input array",
			body: []byte(`{"model":"text-emb","input":[1.0]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for float in input array, got nil", tc.name)
			}
		})
	}
}

// TestIsTokenElement_PathologicalDepth verifies that isTokenElement does not
// stack-overflow on a deeply nested array and instead returns false (which
// causes the caller to fail-closed) once the depth limit is exceeded.
func TestIsTokenElement_PathologicalDepth(t *testing.T) {
	t.Parallel()

	// Build a 200-level deep array: [[[...[1]...]]] with 200 opening brackets.
	// This exceeds maxScanDepth (128), so isTokenElement must return false
	// rather than recursing into a stack overflow.
	const depth = 200
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteByte('[')
	}
	sb.WriteByte('1')
	for i := 0; i < depth; i++ {
		sb.WriteByte(']')
	}
	deepArray := jsonx.RawMessage(sb.String())

	// isTokenElement must return false (reject) rather than crash.
	got := isTokenElement(deepArray, 0)
	if got {
		t.Error("isTokenElement(200-deep array) = true, want false (must reject beyond maxScanDepth)")
	}
}

// TestAnonymizeJSON_PathologicalDepth_NoStackOverflow verifies that
// AnonymizeJSON does not panic or stack-overflow on a pathologically deep
// array in the "input" field. It must either return an error (fail-closed)
// or succeed — but must never crash.
func TestAnonymizeJSON_PathologicalDepth_NoStackOverflow(t *testing.T) {
	t.Parallel()

	// 200-level nested array of 1 in the "input" field.
	const depth = 200
	var sb strings.Builder
	sb.WriteString(`{"model":"text-emb","input":`)
	for i := 0; i < depth; i++ {
		sb.WriteByte('[')
	}
	sb.WriteByte('1')
	for i := 0; i < depth; i++ {
		sb.WriteByte(']')
	}
	sb.WriteByte('}')

	f := newTestFilter(t)
	// Must not panic. Depending on depth vs. maxScanDepth, this may return
	// an error (fail-closed) or succeed. Either is acceptable; what is NOT
	// acceptable is a crash.
	_, _ = f.AnonymizeJSON([]byte(sb.String()))
}

// TestIsTokenElement_ValidIntegers verifies that valid JSON integers are
// accepted as token elements.
func TestIsTokenElement_ValidIntegers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "zero", raw: "0"},
		{name: "positive", raw: "42"},
		{name: "large", raw: "100000"},
		{name: "negative", raw: "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := isTokenElement(jsonx.RawMessage(tc.raw), 0)
			if !got {
				t.Errorf("isTokenElement(%q) = false, want true (valid integer token ID)", tc.raw)
			}
		})
	}
}

// TestAnonymizeJSON_IntTokenArrays_Passthrough verifies that int[] and int[][]
// token arrays in prompt and input are accepted without error and returned unchanged.
func TestAnonymizeJSON_IntTokenArrays_Passthrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "prompt int[] flat",
			body: `{"model":"gpt-3.5","prompt":[1,2,3]}`,
		},
		{
			name: "prompt int[][] nested",
			body: `{"model":"gpt-3.5","prompt":[[1,2],[3,4]]}`,
		},
		{
			name: "input int[] flat",
			body: `{"model":"text-emb","input":[1,2,3,100,200]}`,
		},
		{
			name: "input int[][] nested",
			body: `{"model":"text-emb","input":[[1,2],[3,4]]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out, err := f.AnonymizeJSON([]byte(tc.body))
			if err != nil {
				t.Errorf("[%s] unexpected error for int token array: %v", tc.name, err)
				return
			}
			if f.Touched() {
				t.Errorf("[%s] Touched() = true for int token array, want false", tc.name)
			}
			// The output must be identical to the input (no-op round-trip).
			if string(out) != tc.body {
				t.Errorf("[%s] body was mutated:\n got: %s\nwant: %s", tc.name, out, tc.body)
			}
		})
	}
}

// ── 3. Malformed array-element fail-closed (#2) ───────────────────────────────

// TestAnonymizeJSON_MessagesNonObjectElement_FailClosed verifies that a
// messages array containing a non-object element (including JSON null) triggers
// fail-closed. json.Unmarshal of JSON null into a map returns (nil, nil), so
// the nil-map check is required in addition to the error check.
func TestAnonymizeJSON_MessagesNonObjectElement_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "string element in messages",
			body: []byte(`{"model":"gpt-4","messages":["just a string"]}`),
		},
		{
			name: "integer element in messages",
			body: []byte(`{"model":"gpt-4","messages":[42]}`),
		},
		{
			name: "array element in messages",
			body: []byte(`{"model":"gpt-4","messages":[[1,2,3]]}`),
		},
		{
			name: "null element in messages",
			body: []byte(`{"model":"gpt-4","messages":[null]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for non-object message element, got nil", tc.name)
				return
			}
			// Error must be content-free.
			if strings.Contains(err.Error(), "just a string") {
				t.Errorf("[%s] error leaks body content: %s", tc.name, err.Error())
			}
		})
	}
}

// TestAnonymizeJSON_ToolCallsNonObjectElement_FailClosed verifies that a
// tool_calls array containing a non-object element triggers fail-closed.
func TestAnonymizeJSON_ToolCallsNonObjectElement_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "string element in tool_calls",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":["not-an-object"]}]}`),
		},
		{
			name: "integer element in tool_calls",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[99]}]}`),
		},
		{
			name: "array element in tool_calls",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[[1,2]]}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for non-object tool_calls element, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_ToolsNonObjectElement_FailClosed verifies that a
// top-level tools array containing a non-object element (including JSON null)
// triggers fail-closed. The null case requires a nil-map check in addition to
// the unmarshal error check.
func TestAnonymizeJSON_ToolsNonObjectElement_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "string element in tools",
			body: []byte(`{"model":"gpt-4","messages":[],"tools":["not-an-object"]}`),
		},
		{
			name: "integer element in tools",
			body: []byte(`{"model":"gpt-4","messages":[],"tools":[42]}`),
		},
		{
			name: "null element in tools",
			body: []byte(`{"model":"gpt-4","messages":[],"tools":[null]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for non-object tools element, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_NullArrayFields_FailClosed verifies that covered array
// fields present with value JSON null are rejected fail-closed. A null JSON
// value unmarshalled into a slice produces (nil, nil), so a nil-slice check is
// required alongside the unmarshal error check.
func TestAnonymizeJSON_NullArrayFields_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "messages field is JSON null",
			body: []byte(`{"model":"gpt-4","messages":null}`),
		},
		{
			name: "tools field is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[],"tools":null}`),
		},
		{
			name: "tool_calls field is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":null,"content":"hi"}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for JSON null array field, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_NullObjectFields_FailClosed verifies that covered object
// fields present with value JSON null are rejected fail-closed. A null JSON
// value unmarshalled into a map produces (nil, nil), so a nil-map check is
// required alongside the unmarshal error check.
func TestAnonymizeJSON_NullObjectFields_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "function_call field is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","function_call":null,"content":"hi"}]}`),
		},
		{
			name: "tool_calls element is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[null]}]}`),
		},
		{
			name: "tool_calls[].function is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":null}]}]}`),
		},
		{
			name: "tools[].function is JSON null",
			body: []byte(`{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":null}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for JSON null object field, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_ContentPartNonObject_FailClosed verifies that a content
// array containing a non-object element triggers fail-closed.
func TestAnonymizeJSON_ContentPartNonObject_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "string element in content array",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":["just a string"]}]}`),
		},
		{
			name: "integer element in content array",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[42]}]}`),
		},
		{
			name: "null element in content array",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[null]}]}`),
		},
		{
			name: "boolean element in content array",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[true]}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for non-object content part, got nil", tc.name)
			}
		})
	}
}

// TestAnonymizeJSON_ContentPartNoType_FailClosed verifies that a content-part
// object without a "type" field triggers fail-closed.
func TestAnonymizeJSON_ContentPartNoType_FailClosed(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"text":"hello synthetic@test.invalid"}]}]}`)
	f := newTestFilter(t)
	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Error("expected fail-closed error for content part without type field, got nil")
	}
	if strings.Contains(err.Error(), "synthetic@test.invalid") {
		t.Errorf("error leaks body content: %s", err.Error())
	}
}

// TestAnonymizeJSON_ContentPartUnknownType_FailClosed verifies that a
// content-part with an unknown "type" value triggers fail-closed.
func TestAnonymizeJSON_ContentPartUnknownType_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "type weird",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"weird","text":"synthetic@test.invalid"}]}]}`),
		},
		{
			name: "type custom_blob",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"custom_blob","data":"base64stuff"}]}]}`),
		},
		{
			name: "type empty string",
			body: []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"","text":"something"}]}]}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON(tc.body)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for unknown content part type, got nil", tc.name)
				return
			}
			// Error must not contain body values.
			if strings.Contains(err.Error(), "synthetic@test.invalid") || strings.Contains(err.Error(), "base64stuff") {
				t.Errorf("[%s] error leaks body content: %s", tc.name, err.Error())
			}
		})
	}
}

// TestAnonymizeJSON_ContentPartImageURL_ValidSkipped verifies that a
// content-part with type "image_url" is accepted without error and is passed
// through unchanged (URLs are not PII-scanned).
func TestAnonymizeJSON_ContentPartImageURL_ValidSkipped(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/synth.png"}}]}]}`)
	f := newTestFilter(t)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(image_url part): unexpected error: %v", err)
	}
	// URL must be preserved verbatim.
	if !strings.Contains(string(out), "https://example.test/synth.png") {
		t.Errorf("image_url was modified or removed; got: %s", out)
	}
	if f.Touched() {
		t.Error("Touched() = true for image_url-only content, want false")
	}
}

// TestAnonymizeJSON_KnownNonTextParts_ValidSkipped covers all entries in
// knownContentPartTypes to ensure each is accepted without error and that
// the part is passed through unchanged.
func TestAnonymizeJSON_KnownNonTextParts_ValidSkipped(t *testing.T) {
	t.Parallel()

	knownTypes := []string{
		"image_url",
		"input_audio",
		"input_image",
		"file",
		"image",
		"audio",
		"document",
		"video",
	}

	for _, partType := range knownTypes {
		partType := partType
		t.Run("type_"+partType, func(t *testing.T) {
			t.Parallel()

			body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"` + partType + `","data":"sentinel-value"}]}]}`)
			f := newTestFilter(t)
			out, err := f.AnonymizeJSON(body)
			if err != nil {
				t.Fatalf("AnonymizeJSON(type=%q): unexpected error: %v", partType, err)
			}
			if !strings.Contains(string(out), "sentinel-value") {
				t.Errorf("type=%q: non-text part was modified or removed; got: %s", partType, out)
			}
			if f.Touched() {
				t.Errorf("type=%q: Touched() = true, want false", partType)
			}
		})
	}
}

// TestAnonymizeJSON_ContentPartText_PIIScanned verifies that a content-part
// with type "text" has its "text" field scanned and PII replaced.
func TestAnonymizeJSON_ContentPartText_PIIScanned(t *testing.T) {
	t.Parallel()

	// Use a synthetic email that will be caught by the regex detector.
	const pii = "notify@synthetic-test.example"
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"send to ` + pii + `"}]}]}`)

	f := newTestFilter(t)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(text part): unexpected error: %v", err)
	}
	if strings.Contains(string(out), pii) {
		t.Errorf("content-part text still contains original PII; got: %s", out)
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in text part, want true")
	}
	// Restore must recover the original.
	restored := f.Restore(out)
	if !strings.Contains(string(restored), pii) {
		t.Errorf("restore did not recover original PII; got: %s", restored)
	}
}

// TestAnonymizeJSON_MixedParts_PIIInTextOnly verifies that a content array
// containing both a "text" part (with PII) and an "image_url" part correctly
// anonymizes only the text part.
func TestAnonymizeJSON_MixedParts_PIIInTextOnly(t *testing.T) {
	t.Parallel()

	const pii = "contact@synthetic-mix.example"
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"reach out to ` + pii + `"},` +
		`{"type":"image_url","image_url":{"url":"https://synth.test/pic.jpg"}}` +
		`]}]}`)

	f := newTestFilter(t)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(mixed parts): unexpected error: %v", err)
	}
	// PII in text part must be replaced.
	if strings.Contains(string(out), pii) {
		t.Errorf("text part still contains PII; got: %s", out)
	}
	// image_url part must pass through unchanged.
	if !strings.Contains(string(out), "https://synth.test/pic.jpg") {
		t.Errorf("image URL was removed or modified; got: %s", out)
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in mixed text part, want true")
	}
}
