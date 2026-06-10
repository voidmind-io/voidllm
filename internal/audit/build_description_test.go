package audit

// White-box unit tests for buildDescription and its helpers. These live in
// package audit (not audit_test) so that the unexported buildDescription,
// redactMap, and redactSlice functions are directly accessible.

import (
	"strings"
	"testing"
)

// TestBuildDescription covers the full contract of buildDescription:
// sensitive-field redaction, zero-value dropping, recursive descent, and
// fallback behaviour for invalid input.
func TestBuildDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		// checks is a list of assertions expressed as functions so each test case
		// can assert the combination of contains/not-contains that matters to it.
		checks []func(t *testing.T, got string)
	}{
		// ------------------------------------------------------------------
		// Top-level sensitive field: password
		// ------------------------------------------------------------------
		{
			name: "top-level password is redacted",
			body: `{"email":"a@b.c","password":"secret123"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"email":"a@b.c"`),
				containsStr(`"[REDACTED]"`),
				containsStr(`"password"`),
				notContainsStr("secret123"),
			},
		},
		// ------------------------------------------------------------------
		// All sensitive field names — one subtest per field
		// ------------------------------------------------------------------
		{
			name: "api_key is redacted",
			body: `{"api_key":"sk-live-abc123","name":"my-model"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"api_key"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("sk-live-abc123"),
				containsStr(`"name":"my-model"`),
			},
		},
		{
			name: "auth_token is redacted",
			body: `{"auth_token":"tok-xyz","url":"https://example.com"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"auth_token"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("tok-xyz"),
			},
		},
		{
			name: "oauth_client_secret is redacted",
			body: `{"oauth_client_secret":"oauth-secret-99","client_id":"client-1"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"oauth_client_secret"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("oauth-secret-99"),
				containsStr(`"client_id":"client-1"`),
			},
		},
		{
			name: "client_secret is redacted",
			body: `{"client_secret":"cs-top-secret","issuer":"https://idp.example"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"client_secret"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("cs-top-secret"),
			},
		},
		{
			name: "key is redacted",
			body: `{"key":"vl-license-jwt-xxx","seat_count":5}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"key"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("vl-license-jwt-xxx"),
				containsStr(`"seat_count"`),
			},
		},
		// ------------------------------------------------------------------
		// Nested object: sensitive field inside nested config object
		// ------------------------------------------------------------------
		{
			name: "api_key nested inside config object is redacted",
			body: `{"config":{"api_key":"sk-live-123","name":"x"}}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"api_key"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("sk-live-123"),
				containsStr(`"name":"x"`),
			},
		},
		// ------------------------------------------------------------------
		// Array of objects: sensitive field inside each array element
		// ------------------------------------------------------------------
		{
			name: "auth_token inside array of objects is redacted for all elements",
			body: `{"servers":[{"auth_token":"tok-1","host":"h1"},{"auth_token":"tok-2","host":"h2"}]}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"[REDACTED]"`),
				notContainsStr("tok-1"),
				notContainsStr("tok-2"),
				containsStr(`"host":"h1"`),
				containsStr(`"host":"h2"`),
			},
		},
		// ------------------------------------------------------------------
		// Negative test: "token"-containing field names that are NOT sensitive
		// ------------------------------------------------------------------
		{
			name: "max_tokens and token_count are not redacted (no substring match)",
			body: `{"max_tokens":100,"token_count":5,"prompt":"hello"}`,
			checks: []func(t *testing.T, got string){
				notContainsStr(`"[REDACTED]"`),
				containsStr(`"max_tokens"`),
				containsStr(`"token_count"`),
				// "prompt" is a non-zero string so it is included too
				containsStr(`"prompt":"hello"`),
			},
		},
		// ------------------------------------------------------------------
		// Drop behaviour: zero/empty/null/false values for non-sensitive fields
		// ------------------------------------------------------------------
		{
			name: "zero-value non-sensitive fields are dropped",
			body: `{"name":"","count":0,"flag":false,"x":null,"kept":"value"}`,
			checks: []func(t *testing.T, got string){
				notContainsStr(`"name"`),
				notContainsStr(`"count"`),
				notContainsStr(`"flag"`),
				notContainsStr(`"x"`),
				containsStr(`"kept":"value"`),
			},
		},
		// ------------------------------------------------------------------
		// Sensitive field with empty value: document the actual behaviour.
		// The implementation always emits sensitive fields as "[REDACTED]"
		// regardless of their value, including the empty string.
		// ------------------------------------------------------------------
		{
			name: "password with empty string value is emitted as [REDACTED]",
			body: `{"password":""}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"password"`),
				containsStr(`"[REDACTED]"`),
			},
		},
		// ------------------------------------------------------------------
		// Invalid JSON: returns empty string (existing fallback behaviour)
		// ------------------------------------------------------------------
		{
			name: "invalid JSON returns empty string",
			body: `{not valid json`,
			checks: []func(t *testing.T, got string){
				func(t *testing.T, got string) {
					t.Helper()
					if got != "" {
						t.Errorf("buildDescription(%q) = %q, want empty string for invalid JSON", `{not valid json`, got)
					}
				},
			},
		},
		// ------------------------------------------------------------------
		// Empty body: returns empty string
		// ------------------------------------------------------------------
		{
			name: "empty body returns empty string",
			body: ``,
			checks: []func(t *testing.T, got string){
				func(t *testing.T, got string) {
					t.Helper()
					if got != "" {
						t.Errorf("buildDescription(empty) = %q, want empty string", got)
					}
				},
			},
		},
		// ------------------------------------------------------------------
		// Body that is all-zero after dropping: returns empty string
		// ------------------------------------------------------------------
		{
			name: "body with only zero-value fields returns empty string",
			body: `{"name":"","count":0,"active":false,"data":null}`,
			checks: []func(t *testing.T, got string){
				func(t *testing.T, got string) {
					t.Helper()
					if got != "" {
						t.Errorf("buildDescription(all-zero) = %q, want empty string", got)
					}
				},
			},
		},
		// ------------------------------------------------------------------
		// Multiple sensitive fields at once: every one is redacted
		// ------------------------------------------------------------------
		{
			name: "multiple sensitive fields in same body are all redacted",
			body: `{"password":"p","api_key":"k","name":"bob"}`,
			checks: []func(t *testing.T, got string){
				notContainsStr(`"p"`),
				notContainsStr(`"k"`),
				containsStr(`"password"`),
				containsStr(`"api_key"`),
				containsStr(`"name":"bob"`),
			},
		},
		// ------------------------------------------------------------------
		// Deeply nested: sensitive field two levels deep
		// ------------------------------------------------------------------
		{
			name: "client_secret two levels deep is redacted",
			body: `{"sso":{"provider":"google","credentials":{"client_secret":"deep-secret","client_id":"cid-1"}}}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"client_secret"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("deep-secret"),
				containsStr(`"client_id":"cid-1"`),
			},
		},
		// ------------------------------------------------------------------
		// Array of scalars: scalars pass through unchanged
		// ------------------------------------------------------------------
		{
			name: "array of scalars passes through without redaction",
			body: `{"tags":["alpha","beta"],"active":true}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"tags"`),
				containsStr("alpha"),
				containsStr("beta"),
				notContainsStr(`"[REDACTED]"`),
			},
		},
		// ------------------------------------------------------------------
		// Fix 1 – Case-insensitive redaction
		// ------------------------------------------------------------------
		{
			name: "mixed-case Password field is redacted (case-insensitive)",
			body: `{"email":"a@b.c","Password":"secret"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"email":"a@b.c"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("secret"),
			},
		},
		{
			name: "all-caps API_KEY field is redacted (case-insensitive)",
			body: `{"API_KEY":"sk-upper-case","name":"m"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"[REDACTED]"`),
				notContainsStr("sk-upper-case"),
				containsStr(`"name":"m"`),
			},
		},
		{
			name: "mixed-case Client_Secret field is redacted (case-insensitive)",
			body: `{"Client_Secret":"cs-mixed","issuer":"https://idp.example"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"[REDACTED]"`),
				notContainsStr("cs-mixed"),
			},
		},
		// ------------------------------------------------------------------
		// Fix 2 – "token" field added to sensitiveFields
		// ------------------------------------------------------------------
		{
			name: "token field is redacted",
			body: `{"token":"invite-tok-abc","email":"a@b.c"}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"token"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("invite-tok-abc"),
				containsStr(`"email":"a@b.c"`),
			},
		},
		// ------------------------------------------------------------------
		// Edge-case: sensitive field whose value is itself an object.
		// The sensitiveFields check must fire BEFORE recursion so the nested
		// content is never included in the output.
		// ------------------------------------------------------------------
		{
			name: "sensitive field with object value is redacted without recursing",
			body: `{"password":{"hash":"abc123","algo":"bcrypt"}}`,
			checks: []func(t *testing.T, got string){
				containsStr(`"password"`),
				containsStr(`"[REDACTED]"`),
				notContainsStr("abc123"),
				notContainsStr("bcrypt"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := buildDescription([]byte(tc.body))
			for _, check := range tc.checks {
				check(t, got)
			}
		})
	}
}

// containsStr returns a check function asserting that got contains sub.
func containsStr(sub string) func(t *testing.T, got string) {
	return func(t *testing.T, got string) {
		t.Helper()
		if !strings.Contains(got, sub) {
			t.Errorf("buildDescription output %q does not contain %q", got, sub)
		}
	}
}

// notContainsStr returns a check function asserting that got does NOT contain sub.
func notContainsStr(sub string) func(t *testing.T, got string) {
	return func(t *testing.T, got string) {
		t.Helper()
		if strings.Contains(got, sub) {
			t.Errorf("buildDescription output %q must not contain %q", got, sub)
		}
	}
}
