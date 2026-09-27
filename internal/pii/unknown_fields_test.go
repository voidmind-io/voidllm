package pii

// unknown_fields_test.go covers the default-scan behaviour added to
// anonymizeWithDetectors for top-level fields that are neither explicitly
// covered (coveredTopLevelFields) nor structurally exempt
// (exemptTopLevelFields). It follows the conventions in pii_test.go and
// stop_field_test.go: newTestFilter/mustAnonymize helpers, white-box package
// pii, and the fixed errAnonymizeMsg for fail-closed assertions.

import (
	"strings"
	"testing"
)

// rerankPhone is the exact phone string used in pii_test.go's
// TestRegexDetector_DefaultPatterns_Recognition so its span offsets are known
// to match the PHONE pattern.
const rerankPhone = "+4915112345678"

// rerankIBAN is a syntactically valid (per DefaultPatterns) German IBAN used
// across the unknown-field tests below.
const rerankIBAN = "DE12345678901234567890"

// ── rerank "documents" in every shape providers use ──────────────────────────

// TestFilter_AnonymizeJSON_RerankDocuments_Shapes verifies that the top-level
// "documents" field (not explicitly covered, not exempt) is scanned by
// default regardless of which shape a provider uses for it: plain strings,
// {"text": ...} objects, Cohere-v1-style objects with several string fields,
// and vLLM's {"content":[{"type":"text","text":...}]} content-part shape.
// In every shape, PII is pseudonymized, Restore round-trips it, and sibling
// fields/values that carry no PII are preserved byte-for-byte.
func TestFilter_AnonymizeJSON_RerankDocuments_Shapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantUnchanged []string // substrings that must appear byte-identical in the output
		wantEmailGone bool
		wantIBANGone  bool
		wantEmailBack bool
		wantIBANBack  bool
	}{
		{
			name:          "documents as plain strings",
			body:          `{"model":"rerank-1","query":"q","documents":["contact alice@example.com","IBAN ` + rerankIBAN + ` here"]}`,
			wantUnchanged: nil,
			wantEmailGone: true,
			wantIBANGone:  true,
			wantEmailBack: true,
			wantIBANBack:  true,
		},
		{
			name:          "documents as [{text:...}]",
			body:          `{"model":"rerank-1","query":"q","documents":[{"text":"contact alice@example.com","id":"doc1"}]}`,
			wantUnchanged: []string{`"id":"doc1"`},
			wantEmailGone: true,
			wantEmailBack: true,
		},
		{
			name:          "Cohere-v1 style objects with several string fields",
			body:          `{"model":"rerank-1","query":"q","documents":[{"title":"Report","snippet":"reach me at alice@example.com","url":"https://example.com/doc"}]}`,
			wantUnchanged: []string{`"title":"Report"`, `"url":"https://example.com/doc"`},
			wantEmailGone: true,
			wantEmailBack: true,
		},
		{
			name:          "vLLM content parts",
			body:          `{"model":"rerank-1","query":"q","documents":[{"content":[{"type":"text","text":"contact alice@example.com"}]}]}`,
			wantUnchanged: []string{`"type":"text"`},
			wantEmailGone: true,
			wantEmailBack: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out := mustAnonymize(t, f, []byte(tc.body))

			if tc.wantEmailGone && strings.Contains(string(out), "alice@example.com") {
				t.Errorf("anonymized body still contains original email: %s", out)
			}
			if tc.wantIBANGone && strings.Contains(string(out), rerankIBAN) {
				t.Errorf("anonymized body still contains original IBAN: %s", out)
			}
			if !f.Touched() {
				t.Error("Touched() = false, want true (PII present in documents)")
			}
			for _, sub := range tc.wantUnchanged {
				if !strings.Contains(string(out), sub) {
					t.Errorf("anonymized body missing expected untouched substring %q: %s", sub, out)
				}
			}

			restored := f.Restore(out)
			if tc.wantEmailBack && !strings.Contains(string(restored), "alice@example.com") {
				t.Errorf("restored body missing original email: %s", restored)
			}
			if tc.wantIBANBack && !strings.Contains(string(restored), rerankIBAN) {
				t.Errorf("restored body missing original IBAN: %s", restored)
			}
		})
	}
}

// TestFilter_AnonymizeJSON_RerankDocuments_SplitEmailNotReconstructed verifies
// that an email split across two "documents" array entries ("alice@" and
// "example.com") is never reconstructed by scanning: each string leaf is
// scanned independently, so neither half alone matches the EMAIL pattern and
// both entries survive verbatim with Touched left false.
func TestFilter_AnonymizeJSON_RerankDocuments_SplitEmailNotReconstructed(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"rerank-1","query":"q","documents":["alice@","example.com"]}`)
	out := mustAnonymize(t, f, body)

	if f.Touched() {
		t.Error("Touched() = true for split email halves across documents entries, want false")
	}
	if !strings.Contains(string(out), `"alice@"`) || !strings.Contains(string(out), `"example.com"`) {
		t.Errorf("split entries not preserved verbatim: %s", out)
	}
}

// ── score request fields ─────────────────────────────────────────────────────

// TestFilter_AnonymizeJSON_ScoreFields verifies that the top-level fields used
// by score/similarity requests across providers (text_1/text_2 as strings or
// arrays, queries/items, data_1/data_2) are scanned by default: PII is
// pseudonymized and round-trips through Restore, while sibling values with no
// PII are left byte-identical.
func TestFilter_AnonymizeJSON_ScoreFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantUnchanged []string
	}{
		{
			name:          "text_1/text_2 as strings",
			body:          `{"model":"score-1","text_1":"contact alice@example.com","text_2":"nothing here"}`,
			wantUnchanged: []string{`"text_2":"nothing here"`},
		},
		{
			name:          "text_1/text_2 as arrays",
			body:          `{"model":"score-1","text_1":["contact alice@example.com","clean"],"text_2":["another clean"]}`,
			wantUnchanged: []string{`"clean"`, `"another clean"`},
		},
		{
			name:          "queries/items",
			body:          `{"model":"score-1","queries":["contact alice@example.com"],"items":["clean text a","clean text b"]}`,
			wantUnchanged: []string{`"clean text a"`, `"clean text b"`},
		},
		{
			name:          "data_1/data_2",
			body:          `{"model":"score-1","data_1":"contact alice@example.com","data_2":"clean value"}`,
			wantUnchanged: []string{`"data_2":"clean value"`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out := mustAnonymize(t, f, []byte(tc.body))

			if strings.Contains(string(out), "alice@example.com") {
				t.Errorf("anonymized body still contains original email: %s", out)
			}
			if !f.Touched() {
				t.Error("Touched() = false, want true (PII present)")
			}
			for _, sub := range tc.wantUnchanged {
				if !strings.Contains(string(out), sub) {
					t.Errorf("anonymized body missing expected untouched substring %q: %s", sub, out)
				}
			}

			restored := f.Restore(out)
			if !strings.Contains(string(restored), "alice@example.com") {
				t.Errorf("restored body missing original email: %s", restored)
			}
		})
	}
}

// ── instruction / chat_template_kwargs / metadata ────────────────────────────

// TestFilter_AnonymizeJSON_InstructionChatTemplateKwargsMetadata_Scanned
// verifies that "instruction" (plain string), "chat_template_kwargs" (nested
// object, vLLM-specific), and "metadata" (arbitrary object) are all scanned
// by default: every string leaf containing PII is pseudonymized and restored,
// while non-string leaves (bool, number) at any depth are left untouched.
func TestFilter_AnonymizeJSON_InstructionChatTemplateKwargsMetadata_Scanned(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{` +
		`"model":"vllm-1",` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"instruction":"contact alice@example.com",` +
		`"chat_template_kwargs":{"system_message":"reach bob@example.com","enable_thinking":true,"nested":{"tip":"call ` + rerankPhone + `"}},` +
		`"metadata":{"user_note":"carol@example.com","priority":3}` +
		`}`)
	out := mustAnonymize(t, f, body)

	for _, orig := range []string{"alice@example.com", "bob@example.com", "carol@example.com", rerankPhone} {
		if strings.Contains(string(out), orig) {
			t.Errorf("anonymized body still contains original value %q: %s", orig, out)
		}
	}
	if !f.Touched() {
		t.Error("Touched() = false, want true")
	}
	if !strings.Contains(string(out), `"enable_thinking":true`) {
		t.Errorf("boolean leaf in chat_template_kwargs was altered: %s", out)
	}
	if !strings.Contains(string(out), `"priority":3`) {
		t.Errorf("numeric leaf in metadata was altered: %s", out)
	}

	restored := f.Restore(out)
	for _, orig := range []string{"alice@example.com", "bob@example.com", "carol@example.com", rerankPhone} {
		if !strings.Contains(string(restored), orig) {
			t.Errorf("restored body missing original value %q: %s", orig, restored)
		}
	}
}

// ── exempt fields stay byte-identical, even when PII-shaped ─────────────────

// TestFilter_AnonymizeJSON_ExemptFields_UntouchedEvenWhenPIIShaped verifies
// that every field in exemptTopLevelFields is skipped entirely by the default
// scan — its value reaches the output byte-identical — even when the value
// looks exactly like PII the detectors would otherwise flag. Each body
// contains no PII anywhere else, so the whole body must come back
// byte-for-byte identical to the input and Touched() must stay false.
func TestFilter_AnonymizeJSON_ExemptFields_UntouchedEvenWhenPIIShaped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "model containing an email",
			body: `{"model":"alice@example.com","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "tool_choice with a function name containing an email",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"alice@example.com"}}}`,
		},
		{
			name: "logit_bias with a 13-digit numeric-string key",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"logit_bias":{"1234567890123":-100}}`,
		},
		{
			name: "reasoning_effort containing an email",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"alice@example.com"}`,
		},
		{
			name: "stream_options with a nested field containing an email",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream_options":{"note":"alice@example.com"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			out := mustAnonymize(t, f, []byte(tc.body))

			if f.Touched() {
				t.Errorf("Touched() = true, want false: exempt field must not be scanned; body: %s", out)
			}
			if string(out) != tc.body {
				t.Errorf("AnonymizeJSON mutated a body whose only PII-shaped value is in an exempt field:\ngot:  %s\nwant: %s", out, tc.body)
			}
		})
	}
}

// ── unknown field with PII in an object key: fail-closed ────────────────────

// TestFilter_AnonymizeJSON_UnknownFieldPIIInObjectKey_FailClosed verifies that
// when an unknown (uncovered, non-exempt) top-level field is an object whose
// key itself looks like PII, the request is rejected fail-closed with the
// fixed, caller-content-free error message rather than silently forwarding an
// unscanned or corrupted key.
func TestFilter_AnonymizeJSON_UnknownFieldPIIInObjectKey_FailClosed(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"custom_field":{"alice@example.com":"safe value"}}`)

	_, err := f.AnonymizeJSON(body)
	if err == nil {
		t.Fatal("expected fail-closed error for PII in an unknown field's object key, got nil")
	}
	if err.Error() != errAnonymizeMsg {
		t.Errorf("error = %q, want static message %q", err.Error(), errAnonymizeMsg)
	}
}

// ── unknown field with no PII anywhere: byte-identical no-op ────────────────

// TestFilter_AnonymizeJSON_UnknownFieldNullNumberBool_Untouched verifies that
// unknown top-level fields holding null, numbers, booleans, or nested objects
// of numbers are left completely untouched, and — because nothing else in
// the body carries PII either — the returned body is byte-identical to the
// input (the !touched fast path).
func TestFilter_AnonymizeJSON_UnknownFieldNullNumberBool_Untouched(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"custom_null":null,"custom_num":42,"custom_bool":true,"custom_nested":{"a":{"b":1,"c":false}}}`)
	out := mustAnonymize(t, f, body)

	if f.Touched() {
		t.Errorf("Touched() = true, want false: no PII anywhere in body; body: %s", out)
	}
	if string(out) != string(body) {
		t.Errorf("AnonymizeJSON(no PII) = %s, want byte-identical to input %s", out, body)
	}
}

// ── regression guard: bodies with only covered fields are unaffected ────────

// TestFilter_AnonymizeJSON_CoveredFieldsOnly_RegressionGuard verifies that a
// body containing only explicitly-covered fields (model is exempt, messages
// is covered) behaves exactly as it did before the default unknown-field
// scan was added: the same known-good scenario as
// TestFilter_RoundTrip_StringContent (an email and an IBAN inside
// messages[].content) is pseudonymized and restored identically, proving the
// new default-scan loop is a no-op when there is nothing uncovered to scan.
func TestFilter_AnonymizeJSON_CoveredFieldsOnly_RegressionGuard(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Please contact max.mustermann@example.com about IBAN DE12345678901234567890."}]}`)
	anonBody := mustAnonymize(t, f, body)

	if strings.Contains(string(anonBody), "max.mustermann@example.com") {
		t.Error("anonymized body still contains original email")
	}
	if strings.Contains(string(anonBody), "DE12345678901234567890") {
		t.Error("anonymized body still contains original IBAN")
	}
	if !f.Touched() {
		t.Error("Touched() = false, want true")
	}

	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "max.mustermann@example.com") {
		t.Errorf("restored body does not contain original email; got: %s", restored)
	}
	if !strings.Contains(string(restored), "DE12345678901234567890") {
		t.Errorf("restored body does not contain original IBAN; got: %s", restored)
	}
}
