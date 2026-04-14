package mcp_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestInferSchema verifies that InferSchema returns a correct JSON-Schema-like
// map for a wide range of input values and edge cases.
func TestInferSchema(t *testing.T) {
	t.Parallel()

	// build65Keys constructs a JSON object literal with n integer-keyed properties.
	build65Keys := func() jsonx.RawMessage {
		var sb strings.Builder
		sb.WriteString("{")
		for i := range 65 {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`"k` + strings.Repeat("x", i) + `":"v"`)
		}
		sb.WriteString("}")
		return jsonx.RawMessage(sb.String())
	}

	tests := []struct {
		name    string
		input   jsonx.RawMessage
		wantNil bool
		want    map[string]any
	}{
		{
			name:  "string primitive",
			input: jsonx.RawMessage(`"hello"`),
			want:  map[string]any{"type": "string"},
		},
		{
			name:  "integer number primitive",
			input: jsonx.RawMessage(`42`),
			want:  map[string]any{"type": "number"},
		},
		{
			name:  "float number primitive",
			input: jsonx.RawMessage(`3.14`),
			want:  map[string]any{"type": "number"},
		},
		{
			name:  "boolean true primitive",
			input: jsonx.RawMessage(`true`),
			want:  map[string]any{"type": "boolean"},
		},
		{
			name:  "boolean false primitive",
			input: jsonx.RawMessage(`false`),
			want:  map[string]any{"type": "boolean"},
		},
		{
			name:  "null falls back to string",
			input: jsonx.RawMessage(`null`),
			want:  map[string]any{"type": "string"},
		},
		{
			name:    "InvalidJSON returns nil",
			input:   jsonx.RawMessage(`not json at all`),
			wantNil: true,
		},
		{
			name:  "EmptyObject",
			input: jsonx.RawMessage(`{}`),
			want:  map[string]any{"type": "object"},
		},
		{
			name:  "EmptyArray",
			input: jsonx.RawMessage(`[]`),
			want:  map[string]any{"type": "array", "items": map[string]any{}},
		},
		{
			name:  "ArrayOfStrings",
			input: jsonx.RawMessage(`["a", "b"]`),
			want:  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		{
			name:  "ArrayOfNumbers",
			input: jsonx.RawMessage(`[1, 2, 3]`),
			want:  map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
		},
		{
			name:  "ArrayOfBooleans",
			input: jsonx.RawMessage(`[true, false]`),
			want:  map[string]any{"type": "array", "items": map[string]any{"type": "boolean"}},
		},
		{
			name:  "ObjectWithMixedProperties",
			input: jsonx.RawMessage(`{"name": "alice", "age": 30, "active": true}`),
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string"},
					"age":    map[string]any{"type": "number"},
					"active": map[string]any{"type": "boolean"},
				},
			},
		},
		{
			name:  "NestedObject",
			input: jsonx.RawMessage(`{"user": {"id": 1, "name": "a"}}`),
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":   map[string]any{"type": "number"},
							"name": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
		{
			// maxSchemaDepth = 3. An object 4 levels deep means the innermost
			// object is encountered at depth 4 (> 3) and is truncated.
			// Structure: {"a": {"b": {"c": {"d": "leaf"}}}}
			//   depth 0: outer object     → has properties
			//   depth 1: "a" object       → has properties
			//   depth 2: "b" object       → has properties
			//   depth 3: "c" object       → has properties (depth == maxSchemaDepth, not > it)
			//   depth 4: "d" leaf value   → this call has depth > maxSchemaDepth → truncated
			// Wait — the depth check is `depth > maxSchemaDepth` before the switch.
			// At depth 3 (the "c" object), we enter the map[string]any case and recurse
			// into its child at depth 4. depth 4 > 3 → truncated to {type: "object"}.
			name:  "DepthLimit",
			input: jsonx.RawMessage(`{"a": {"b": {"c": {"d": "leaf"}}}}`),
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"b": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"c": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"d": map[string]any{"type": "object"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			// 65 keys exceeds maxSchemaProperties = 64 → collapses to {type: "object"}
			name:  "TooManyProperties",
			input: build65Keys(),
			want:  map[string]any{"type": "object"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.InferSchema(tc.input)

			if tc.wantNil {
				if got != nil {
					t.Fatalf("InferSchema(%q) = %s, want nil", tc.input, got)
				}
				return
			}

			var gotMap map[string]any
			if err := jsonx.Unmarshal(got, &gotMap); err != nil {
				t.Fatalf("InferSchema(%q): unmarshal result %q: %v", tc.input, got, err)
			}

			if !reflect.DeepEqual(gotMap, tc.want) {
				t.Errorf("InferSchema(%q)\n  got  = %v\n  want = %v", tc.input, gotMap, tc.want)
			}
		})
	}
}

// buildDeepArraySchema returns a JSON schema for an array nested n levels deep,
// with "string" at the innermost items. At n=71 the rendered TypeScript type
// "Array<Array<...<string>...>>" is 503 characters, exceeding maxPropTypeLen=500.
func buildDeepArraySchema(n int) jsonx.RawMessage {
	// Build from the inside out: start with {"type":"string"}, then wrap n times.
	inner := `{"type":"string"}`
	for range n {
		inner = `{"type":"array","items":` + inner + `}`
	}
	return jsonx.RawMessage(inner)
}

// TestSchemaToTypeScript verifies the TypeScript type-string generation for a
// range of JSON-Schema-like inputs.
func TestSchemaToTypeScript(t *testing.T) {
	t.Parallel()

	// oversizedPropSchema builds an object schema whose single valid property
	// renders to a TypeScript type longer than maxPropTypeLen (500) characters.
	// At 71 nesting levels: len("Array<")*71 + len("string") + 71 = 6*71+6+71 = 503 > 500.
	oversizedPropSchema := func() jsonx.RawMessage {
		itemsSchema := buildDeepArraySchema(71)
		return jsonx.RawMessage(`{"type":"object","properties":{"bigprop":` + string(itemsSchema) + `}}`)
	}()

	tests := []struct {
		name  string
		input jsonx.RawMessage
		want  string
	}{
		{
			name:  "string type",
			input: jsonx.RawMessage(`{"type":"string"}`),
			want:  "string",
		},
		{
			name:  "number type",
			input: jsonx.RawMessage(`{"type":"number"}`),
			want:  "number",
		},
		{
			name:  "boolean type",
			input: jsonx.RawMessage(`{"type":"boolean"}`),
			want:  "boolean",
		},
		{
			name:  "array of strings",
			input: jsonx.RawMessage(`{"type":"array","items":{"type":"string"}}`),
			want:  "Array<string>",
		},
		{
			name:  "array without items",
			input: jsonx.RawMessage(`{"type":"array"}`),
			want:  "any[]",
		},
		{
			name:  "object with properties sorted alphabetically",
			input: jsonx.RawMessage(`{"type":"object","properties":{"id":{"type":"number"},"name":{"type":"string"}}}`),
			want:  "{ id: number; name: string }",
		},
		{
			name:  "object without properties",
			input: jsonx.RawMessage(`{"type":"object"}`),
			want:  "Record<string, any>",
		},
		{
			name:  "array of objects",
			input: jsonx.RawMessage(`{"type":"array","items":{"type":"object","properties":{"id":{"type":"number"}}}}`),
			want:  "Array<{ id: number }>",
		},
		{
			name:  "unknown type",
			input: jsonx.RawMessage(`{"type":"date"}`),
			want:  "any",
		},
		{
			name:  "invalid JSON",
			input: jsonx.RawMessage(`not valid json`),
			want:  "any",
		},
		// --- Security regression cases ---
		//
		// These cases exercise the injection-prevention guards in schemaTypeToTS.
		// If a future refactor removes isValidJSIdentifier filtering or the
		// maxPropTypeLen cap, these tests will fail.
		{
			// An injection-payload key ("*/\nIGNORE") must be dropped; the valid
			// key survives and the output is a clean TypeScript literal with no
			// injected content.
			name: "ObjectWithInvalidPropertyName_DropsKey",
			input: jsonx.RawMessage(`{"type":"object","properties":{` +
				`"valid_key":{"type":"string"},` +
				`"*/\nIGNORE":{"type":"number"}` +
				`}}`),
			want: "{ valid_key: string }",
		},
		{
			// Both keys fail isValidJSIdentifier ("*/" contains '*' and '/';
			// "has space" contains a space). After filtering, no valid keys
			// remain — the output must be the safe fallback Record<string, any>.
			name: "ObjectWithAllInvalidPropertyNames_Record",
			input: jsonx.RawMessage(`{"type":"object","properties":{` +
				`"*/":{"type":"number"},` +
				`"has space":{"type":"string"}` +
				`}}`),
			want: "Record<string, any>",
		},
		{
			// A hyphen is not a valid JS identifier character. "kebab-case" must
			// be dropped; "valid" survives.
			name: "ObjectWithHyphenatedProperty_DropsKey",
			input: jsonx.RawMessage(`{"type":"object","properties":{` +
				`"valid":{"type":"string"},` +
				`"kebab-case":{"type":"number"}` +
				`}}`),
			want: "{ valid: string }",
		},
		{
			// A newline embedded in a property name must be dropped; "ok" survives.
			name: "ObjectWithNewlineInPropertyName_DropsKey",
			input: jsonx.RawMessage("{\"type\":\"object\",\"properties\":{" +
				"\"ok\":{\"type\":\"string\"}," +
				"\"bad\\nkey\":{\"type\":\"boolean\"}" +
				"}}"),
			want: "{ ok: string }",
		},
		{
			// A property whose rendered TypeScript type exceeds maxPropTypeLen
			// (500 chars) must be replaced with ": any". We verify the output
			// contains "bigprop: any" rather than the full unbounded type string.
			// This ensures the length cap is enforced and prevents description bloat
			// from crafted deeply-nested schemas injected by a compromised MCP server.
			name:  "ObjectWithPropertyValueExceedsLengthCap_RendersAny",
			input: oversizedPropSchema,
			want:  "{ bigprop: any }",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.SchemaToTypeScript(tc.input)
			if got != tc.want {
				t.Errorf("SchemaToTypeScript(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestInferSchema_RejectsPathologicalDepth verifies the pre-unmarshal bracket
// depth guard in InferSchema. Inputs with bracket nesting > maxInputDepth (64)
// are rejected before sonic allocates memory, bounding CPU and heap usage on
// adversarial payloads. A positive control at 60 levels confirms the guard does
// not over-fire on legitimate (if deep) inputs.
func TestInferSchema_RejectsPathologicalDepth(t *testing.T) {
	t.Parallel()

	t.Run("depth_70_rejected", func(t *testing.T) {
		t.Parallel()

		// 70 nested arrays exceeds maxInputDepth = 64.
		input := jsonx.RawMessage(strings.Repeat("[", 70) + strings.Repeat("]", 70))
		got := mcp.InferSchema(input)
		if got != nil {
			t.Errorf("InferSchema(depth=70): expected nil (guard fired), got %q", got)
		}
	})

	t.Run("depth_60_allowed", func(t *testing.T) {
		t.Parallel()

		// 60 nested arrays is below maxInputDepth = 64. InferSchema proceeds past
		// the depth guard. inferValue truncates at maxSchemaDepth=3, so the result
		// is non-nil (some schema is produced).
		input := jsonx.RawMessage(strings.Repeat("[", 60) + strings.Repeat("]", 60))
		got := mcp.InferSchema(input)
		if got == nil {
			t.Error("InferSchema(depth=60): expected non-nil result (guard should not fire), got nil")
		}
	})
}

// TestInferSchema_DepthScanIgnoresStringContents documents a known, deliberate
// trade-off in the exceedsMaxDepth byte-scan: bracket characters inside JSON
// string values are counted as nesting depth, which can cause the guard to fire
// on inputs whose actual structural depth is shallow.
//
// This conservative over-count is intentional for DoS prevention. The scan
// avoids the cost of a full parse and ensures worst-case work is bounded by a
// simple linear scan. A "fix" that skips string contents would require parsing
// quotes and escape sequences, reintroducing the attack surface this guard was
// designed to close.
//
// This test locks the existing behavior: if someone changes exceedsMaxDepth to
// parse strings and thereby allow the input below through, this test will catch
// the regression.
func TestInferSchema_DepthScanIgnoresStringContents(t *testing.T) {
	t.Parallel()

	// The outer JSON object has structural depth 1 (one '{' ... '}').
	// However, the string value contains 100 '[' characters, pushing the
	// naive bracket counter above maxInputDepth=64. InferSchema must return nil.
	brackets := strings.Repeat("[", 100)
	input := jsonx.RawMessage(`{"note":"` + brackets + `"}`)

	got := mcp.InferSchema(input)
	if got != nil {
		// If this fires, exceedsMaxDepth now parses strings — the DoS guard has
		// been weakened. Restore the simple byte scan.
		t.Errorf("InferSchema(string with 100 brackets): expected nil (conservative over-count), got %q", got)
	}
}
