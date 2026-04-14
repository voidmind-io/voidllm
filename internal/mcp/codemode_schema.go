package mcp

import (
	"sort"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

const (
	maxSchemaDepth      = 3
	maxSchemaProperties = 64
	// maxInputDepth is the maximum bracket nesting depth (counting '[' and '{')
	// tolerated by the pre-unmarshal depth scan in InferSchema. Inputs that
	// exceed this limit are rejected before sonic allocates any memory for
	// them, bounding CPU and heap usage on adversarial payloads.
	maxInputDepth = 64
	// maxPropTypeLen is the maximum length of a rendered TypeScript type string
	// for a single object property. Values that exceed this are replaced with
	// "any" to prevent description bloat from crafted deeply-nested schemas.
	maxPropTypeLen = 500
)

// InferSchema walks a JSON value and produces a JSON-Schema-like map.
// Max depth 3 — deeper objects become {"type": "object"}.
// Objects with more than 64 properties collapse to {"type": "object"}.
// Returns nil if raw is not valid JSON or if the raw bytes exceed
// maxInputDepth bracket nesting (checked before unmarshalling to bound CPU
// and memory usage on adversarial inputs).
func InferSchema(raw jsonx.RawMessage) jsonx.RawMessage {
	if exceedsMaxDepth(raw, maxInputDepth) {
		return nil
	}
	var v any
	if err := jsonx.Unmarshal(raw, &v); err != nil {
		return nil
	}
	schema := inferValue(v, 0)
	out, _ := jsonx.Marshal(schema)
	return out
}

// exceedsMaxDepth reports whether raw contains a bracket nesting run longer
// than limit. It counts '[' and '{' without parsing string contents, so a
// string value like "[[[" inflates the count. This conservative over-count is
// intentional: the purpose is DoS prevention, not exactness.
func exceedsMaxDepth(raw []byte, limit int) bool {
	depth := 0
	for _, b := range raw {
		switch b {
		case '[', '{':
			depth++
			if depth > limit {
				return true
			}
		case ']', '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return false
}

func inferValue(v any, depth int) map[string]any {
	if depth > maxSchemaDepth {
		return map[string]any{"type": "object"}
	}
	switch val := v.(type) {
	case nil:
		return map[string]any{"type": "string"} // safe fallback
	case bool:
		return map[string]any{"type": "boolean"}
	case float64:
		return map[string]any{"type": "number"}
	case string:
		return map[string]any{"type": "string"}
	case []any:
		if len(val) == 0 {
			return map[string]any{"type": "array", "items": map[string]any{}}
		}
		return map[string]any{"type": "array", "items": inferValue(val[0], depth+1)}
	case map[string]any:
		if len(val) == 0 {
			return map[string]any{"type": "object"}
		}
		if len(val) > maxSchemaProperties {
			return map[string]any{"type": "object"}
		}
		props := make(map[string]any, len(val))
		for k, child := range val {
			props[k] = inferValue(child, depth+1)
		}
		return map[string]any{"type": "object", "properties": props}
	default:
		return map[string]any{"type": "any"}
	}
}

// schemaToTypeScript converts an inferred JSON-Schema-like object to a
// TypeScript type string.
func schemaToTypeScript(schema jsonx.RawMessage) string {
	var s map[string]any
	if err := jsonx.Unmarshal(schema, &s); err != nil {
		return "any"
	}
	return schemaTypeToTS(s)
}

// schemaTypeToTS recursively maps a parsed JSON Schema map to a TypeScript
// type string. In the object branch, property names that are not valid JS
// identifiers (as determined by isValidJSIdentifier) are silently dropped to
// prevent a compromised upstream MCP server from injecting prompt-injection
// payloads into the LLM-visible tool description via crafted property names.
// If all properties are dropped the result falls back to Record<string, any>.
// Property type strings longer than maxPropTypeLen characters are rendered as
// "any" to prevent description bloat from pathologically wide schemas.
func schemaTypeToTS(s map[string]any) string {
	typ, _ := s["type"].(string)
	switch typ {
	case "string":
		return "string"
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		items, ok := s["items"].(map[string]any)
		if !ok {
			return "any[]"
		}
		return "Array<" + schemaTypeToTS(items) + ">"
	case "object":
		props, ok := s["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			return "Record<string, any>"
		}
		// Collect only valid JS identifier keys to prevent injection via
		// crafted property names. Sort for deterministic output.
		keys := make([]string, 0, len(props))
		for k := range props {
			if isValidJSIdentifier(k) {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return "Record<string, any>"
		}
		sort.Strings(keys)

		var sb strings.Builder
		sb.WriteString("{ ")
		for i, k := range keys {
			if i > 0 {
				sb.WriteString("; ")
			}
			propSchema, ok := props[k].(map[string]any)
			if !ok {
				sb.WriteString(k + ": any")
				continue
			}
			propType := schemaTypeToTS(propSchema)
			if len(propType) > maxPropTypeLen {
				propType = "any"
			}
			sb.WriteString(k + ": " + propType)
		}
		sb.WriteString(" }")
		return sb.String()
	default:
		return "any"
	}
}
