package pii

// stop_field_test.go covers the top-level "stop" field, which is scanned for
// every provider regardless of endpoint (see anonymizeWithDetectors's doc).
// It follows the conventions in pii_test.go: newTestFilter/mustAnonymize
// helpers, white-box package pii so the fixed secret and org ID are shared.

import (
	"strings"
	"testing"
)

// errAnonymizeMsg is the fixed, caller-content-free error message every
// fail-closed path in anonymizeWithDetectors returns.
const errAnonymizeMsg = "pii: request body could not be processed for anonymization"

// TestFilter_AnonymizeJSON_StopField_String verifies that a plain-string stop
// value is pseudonymized and round-trips through Restore.
func TestFilter_AnonymizeJSON_StopField_String(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":"contact alice@example.com"}`)
	out := mustAnonymize(t, f, body)

	if strings.Contains(string(out), "alice@example.com") {
		t.Errorf("anonymized body still contains original email in stop field: %s", out)
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in stop field, want true")
	}
	restored := f.Restore(out)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restored body missing original email: %s", restored)
	}
}

// TestFilter_AnonymizeJSON_StopField_ArraySplitNotReconstructed verifies that
// each array entry is pseudonymized independently: an email split across two
// entries ("alice@" and "example.com") never gets concatenated back into a
// detectable email during scanning — neither half alone matches the EMAIL
// pattern, so both entries survive verbatim and Touched stays false.
func TestFilter_AnonymizeJSON_StopField_ArraySplitNotReconstructed(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":["alice@","example.com"]}`)
	out := mustAnonymize(t, f, body)

	if f.Touched() {
		t.Error("Touched() = true for split email halves with no full email in either entry, want false")
	}
	if !strings.Contains(string(out), `"alice@"`) || !strings.Contains(string(out), `"example.com"`) {
		t.Errorf("split entries not preserved verbatim: %s", out)
	}
}

// TestFilter_AnonymizeJSON_StopField_ArrayEachEntryIndependent verifies that
// when one array entry genuinely contains PII, only that entry is
// pseudonymized; a sibling entry with no PII is left untouched.
func TestFilter_AnonymizeJSON_StopField_ArrayEachEntryIndependent(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":["STOP","bob@example.com"]}`)
	out := mustAnonymize(t, f, body)

	if strings.Contains(string(out), "bob@example.com") {
		t.Errorf("anonymized body still contains original email in stop array entry: %s", out)
	}
	if !strings.Contains(string(out), `"STOP"`) {
		t.Errorf("non-PII array entry not preserved verbatim: %s", out)
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in a stop array entry, want true")
	}
}

// TestFilter_AnonymizeJSON_StopField_NullIsNoOp verifies that a JSON null stop
// value is a no-op: the body is returned unmodified and nothing is marked
// touched.
func TestFilter_AnonymizeJSON_StopField_NullIsNoOp(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":null}`)
	out := mustAnonymize(t, f, body)

	if string(out) != string(body) {
		t.Errorf("AnonymizeJSON(null stop) mutated body: got %s, want %s", out, body)
	}
	if f.Touched() {
		t.Error("Touched() = true for null stop, want false")
	}
}

// TestFilter_AnonymizeJSON_StopField_FailClosedShapes verifies that every
// shape "stop" has no defined meaning for — a bare number, an object, or an
// array containing a non-string element — is rejected fail-closed with the
// fixed, caller-content-free error message rather than being silently
// forwarded or dropped.
func TestFilter_AnonymizeJSON_StopField_FailClosedShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"number", `{"model":"gpt-4","messages":[],"stop":42}`},
		{"object", `{"model":"gpt-4","messages":[],"stop":{"x":1}}`},
		{"array with a number element", `{"model":"gpt-4","messages":[],"stop":["STOP",1]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t)
			_, err := f.AnonymizeJSON([]byte(tc.body))
			if err == nil {
				t.Fatalf("expected fail-closed error for stop shape %q, got nil", tc.name)
			}
			if err.Error() != errAnonymizeMsg {
				t.Errorf("error = %q, want static message %q", err.Error(), errAnonymizeMsg)
			}
		})
	}
}

// TestFilter_AnonymizeJSON_StopField_RestorationNotNeeded verifies that "stop"
// is a request-only field with no response-path counterpart: anonymizing a
// stop value does not affect Restore's behavior on unrelated response bytes
// that contain neither the pseudonym nor the original value.
func TestFilter_AnonymizeJSON_StopField_RestorationNotNeeded(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stop":"contact carol@example.com"}`)
	_ = mustAnonymize(t, f, body)

	respWithoutPseudonym := []byte(`{"choices":[{"message":{"content":"no stop info here"}}]}`)
	restored := f.Restore(respWithoutPseudonym)
	if string(restored) != string(respWithoutPseudonym) {
		t.Errorf("Restore altered an unrelated response body: got %s, want %s", restored, respWithoutPseudonym)
	}
}
