package mcp

import (
	"sort"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// GenerateToolTypeDefs produces TypeScript-style namespace declarations for all
// tools in serverTools. The output is intended to be embedded in the
// execute_code tool description so an LLM can infer correct call syntax and
// argument shapes without calling search_tools first.
//
// Each server alias becomes a namespace under the global tools object:
//
//	declare namespace tools.aws {
//	  /** Short description */
//	  function search_documentation(args: { query: string; limit?: number }): Promise<any>;
//	}
//
// outputSchemas maps server alias → tool name → inferred output schema
// (as returned by InferSchema). When a schema is present for a tool, the
// return type is emitted as a concrete TypeScript type instead of Promise<any>.
// A nil outputSchemas map is valid and produces the same output as before —
// all tool signatures emit Promise<any>.
//
// If serverTools is empty, GenerateToolTypeDefs returns an empty string.
func GenerateToolTypeDefs(serverTools map[string][]Tool, outputSchemas map[string]map[string]jsonx.RawMessage) string {
	if len(serverTools) == 0 {
		return ""
	}

	// Sort aliases for deterministic output.
	aliases := make([]string, 0, len(serverTools))
	for alias := range serverTools {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	var sb strings.Builder
	for i, alias := range aliases {
		tools := serverTools[alias]
		if len(tools) == 0 {
			continue
		}

		if i > 0 {
			sb.WriteByte('\n')
		}

		// Use bracket notation for aliases with special characters so the
		// TypeScript-style declaration remains syntactically plausible.
		if isValidJSIdentifier(alias) {
			sb.WriteString("declare namespace tools.")
			sb.WriteString(alias)
			sb.WriteString(" {\n")
		} else {
			sb.WriteString("// tools[\"")
			sb.WriteString(alias)
			sb.WriteString("\"]\ndeclare namespace tools_")
			// Replace non-alphanum with underscore for the namespace name
			for _, r := range alias {
				if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
					sb.WriteRune(r)
				} else {
					sb.WriteRune('_')
				}
			}
			sb.WriteString(" {\n")
		}

		// Sort tools within each namespace for deterministic output.
		sorted := make([]Tool, len(tools))
		copy(sorted, tools)
		sort.Slice(sorted, func(a, b int) bool {
			return sorted[a].Name < sorted[b].Name
		})

		aliasSchemas := outputSchemas[alias] // nil when outputSchemas is nil or alias absent
		for _, tool := range sorted {
			var outSchema jsonx.RawMessage
			if aliasSchemas != nil {
				outSchema = aliasSchemas[tool.Name]
			}
			writeToolDecl(&sb, tool, outSchema)
		}

		sb.WriteString("}")
	}
	return sb.String()
}

// parseObjectSchema extracts the top-level "properties" and "required"
// keywords from an object JSONSchema for TypeScript declaration rendering.
// Each property is returned as its raw parsed JSON Schema keywords rather
// than a typed struct — writeToolDecl only reads "type" from it, rendering a
// shallow, non-recursive argument list, consistent with how this function's
// output has always been rendered. Malformed or missing schemas yield nil
// results rather than an error: the enclosing description is best-effort
// documentation for an LLM, not a validated contract.
func parseObjectSchema(schema JSONSchema) (props map[string]map[string]any, required map[string]bool) {
	var parsed struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := jsonx.Unmarshal(schema, &parsed); err != nil {
		return nil, nil
	}
	required = make(map[string]bool, len(parsed.Required))
	for _, r := range parsed.Required {
		required[r] = true
	}
	return parsed.Properties, required
}

// writeToolDecl writes a single TypeScript function declaration into sb.
// outSchema is the inferred output schema for the tool; when non-nil the
// return type is a concrete TypeScript type with a comment noting it is
// inferred. When nil, Promise<any> is emitted.
func writeToolDecl(sb *strings.Builder, tool Tool, outSchema jsonx.RawMessage) {
	// Skip tools whose names are not valid identifiers — they could inject
	// arbitrary content into the TypeScript declaration block.
	if !isValidJSIdentifier(tool.Name) {
		// Emit a comment so the LLM knows the tool exists but use bracket notation.
		sb.WriteString("  // tools[\"<alias>\"][\"")
		sb.WriteString(strings.ReplaceAll(tool.Name, "\"", "\\\""))
		sb.WriteString("\"](args)\n")
		return
	}

	// JSDoc comment with truncated description (newlines stripped to prevent
	// breaking out of the JSDoc block).
	if desc := truncateDescription(tool.Description); desc != "" {
		clean := strings.ReplaceAll(desc, "*/", "")
		clean = strings.ReplaceAll(clean, "\n", " ")
		sb.WriteString("  /** ")
		sb.WriteString(clean)
		sb.WriteString(" */\n")
	}

	sb.WriteString("  function ")
	sb.WriteString(tool.Name)
	sb.WriteString("(args: {")

	props, required := parseObjectSchema(tool.InputSchema)
	if len(props) > 0 {
		// Sort property names for deterministic output.
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)

		sb.WriteByte('\n')
		for _, name := range names {
			propType, _ := props[name]["type"].(string)
			sb.WriteString("    ")
			sb.WriteString(name)
			if !required[name] {
				sb.WriteByte('?')
			}
			sb.WriteString(": ")
			sb.WriteString(tsType(propType))
			sb.WriteString(";\n")
		}
		sb.WriteString("  ")
	}

	if outSchema != nil {
		sb.WriteString("}): Promise<")
		sb.WriteString(schemaToTypeScript(outSchema))
		sb.WriteString(">; // inferred - could depend on previous query\n")
	} else {
		sb.WriteString("}): Promise<any>;\n")
	}
}

// isValidJSIdentifier reports whether s is a valid bare JavaScript identifier
// (only ASCII letters, digits, underscore, not starting with a digit).
func isValidJSIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if r >= '0' && r <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}

// tsType maps a JSON Schema type string to the corresponding TypeScript type.
func tsType(jsonType string) string {
	switch jsonType {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		return "any[]"
	case "object":
		return "Record<string, any>"
	default:
		return "any"
	}
}

// truncateDescription returns the first sentence of desc. A sentence boundary
// is the first occurrence of ". " (period-space) or a newline character. The
// trailing period, if any, is preserved. If desc contains no sentence boundary,
// the entire string is returned unchanged.
func truncateDescription(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}

	// Locate the first sentence boundary.
	if idx := strings.Index(desc, ". "); idx >= 0 {
		return desc[:idx+1]
	}
	if idx := strings.IndexByte(desc, '\n'); idx >= 0 {
		return strings.TrimSpace(desc[:idx])
	}
	return desc
}
