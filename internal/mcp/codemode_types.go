package mcp

import (
	"sort"
	"strings"
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
// If serverTools is empty, GenerateToolTypeDefs returns an empty string.
func GenerateToolTypeDefs(serverTools map[string][]Tool) string {
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

		for _, tool := range sorted {
			writeToolDecl(&sb, tool)
		}

		sb.WriteString("}")
	}
	return sb.String()
}

// writeToolDecl writes a single TypeScript function declaration into sb.
func writeToolDecl(sb *strings.Builder, tool Tool) {
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

	props := tool.InputSchema.Properties
	if len(props) > 0 {
		required := make(map[string]bool, len(tool.InputSchema.Required))
		for _, r := range tool.InputSchema.Required {
			required[r] = true
		}

		// Sort property names for deterministic output.
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)

		sb.WriteByte('\n')
		for _, name := range names {
			prop := props[name]
			sb.WriteString("    ")
			sb.WriteString(name)
			if !required[name] {
				sb.WriteByte('?')
			}
			sb.WriteString(": ")
			sb.WriteString(tsType(prop.Type))
			sb.WriteString(";\n")
		}
		sb.WriteString("  ")
	}

	sb.WriteString("}): Promise<any>;\n")
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
