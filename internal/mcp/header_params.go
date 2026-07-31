package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// HeaderParamKind identifies which of the three JSON Schema primitive types
// an x-mcp-header-annotated tool-input-schema property may declare, per MCP
// 2026-07-28 §4.3. "number" is deliberately not among them, even though it is
// otherwise a JSON Schema primitive type: a number's text representation is
// not unique — "1e3", "1000.0", and "1000" all denote the same value — so a
// server comparing an Mcp-Param-{Name} header against the request body
// (§4.5) could never reliably tell whether the two actually agree without
// first parsing both as numbers, which defeats the point of a header a
// load balancer or gateway is supposed to be able to act on without parsing
// the body at all (§4.2's own stated purpose for the header family).
type HeaderParamKind int

// The three HeaderParamKind values MCP 2026-07-28 §4.3 permits an
// x-mcp-header annotation to sit on.
const (
	// HeaderParamKindString marks a property whose JSON Schema "type" is
	// exactly the string "string".
	HeaderParamKindString HeaderParamKind = iota + 1
	// HeaderParamKindInteger marks a property whose JSON Schema "type" is
	// exactly the string "integer".
	HeaderParamKindInteger
	// HeaderParamKindBoolean marks a property whose JSON Schema "type" is
	// exactly the string "boolean".
	HeaderParamKindBoolean
)

// HeaderParam is one validated x-mcp-header binding: a tool-input-schema
// property that a server has annotated for header mirroring (MCP 2026-07-28
// §4.3) and that ToolHeaderParams has confirmed satisfies every constraint
// the spec places on that annotation.
type HeaderParam struct {
	// Name is the x-mcp-header annotation's own value — the suffix mirrored
	// onto the wire as HeaderParamPrefix + Name (Mcp-Param-{Name}).
	Name string
	// Path is the chain of "properties" keys from the schema's root to the
	// annotated property — e.g. []string{"region"} for a top-level property,
	// or []string{"filter", "region"} for one nested one level deeper. Every
	// element is a plain "properties" key; a HeaderParam's Path never crosses
	// items, oneOf/anyOf/allOf/not, if/then/else, or a $ref, because
	// ToolHeaderParams rejects any annotation reached that way (see its own
	// doc for "statically reachable").
	Path []string
	// Kind is the property's declared primitive type: HeaderParamKindString,
	// HeaderParamKindInteger, or HeaderParamKindBoolean.
	Kind HeaderParamKind
}

// ErrHeaderParamConstraint is the sentinel every error ToolHeaderParams
// returns wraps. It never indicates that no x-mcp-header annotation was
// found — an unannotated schema simply returns a nil slice and a nil error —
// only that at least one annotation present somewhere in the document
// violates MCP 2026-07-28 §4.3.
var ErrHeaderParamConstraint = errors.New("mcp: x-mcp-header annotation violates MCP 2026-07-28 §4.3")

// maxHeaderParamWalkDepth and maxHeaderParamWalkNodes bound ToolHeaderParams'
// walk of a tool's input schema: at most this many "properties"/composition
// levels deep, and at most this many total JSON values visited (objects,
// arrays, and scalars all count), before the walk gives up rather than
// finishing. A schema that exceeds either bound has its owning tool EXCLUDED
// from tools/list, exactly like a schema with a genuine constraint
// violation — never included on the theory that "we simply stopped
// checking". That is the deliberately pessimistic direction: an
// x-mcp-header annotation this package cannot prove satisfies every §4.3
// constraint, because the walk needed to prove it exceeded a bound, is an
// annotation this package cannot claim to have fully validated — and MCP
// 2026-07-28 §4.3 places the burden of that proof on the client ("Clients
// MÜSSEN das unterstützen"), not on the server that wrote the schema.
// Passing an unverified annotation through would misrepresent VoidLLM's own
// compliance; excluding the tool is the only response that does not. Both
// bounds are otherwise generous for any schema this project's own tools or a
// well-behaved upstream produce — see maxSchemaDepth/maxSchemaProperties in
// codemode_schema.go for the sibling reasoning applied to a different schema
// walk in this package.
const (
	maxHeaderParamWalkDepth = 16
	maxHeaderParamWalkNodes = 4096
)

// headerParamWalker accumulates the state a single ToolHeaderParams call
// needs across its recursive walk: the case-insensitive set of annotation
// names already seen (for duplicate detection), the bindings collected so
// far, and a running count of JSON values visited (for maxHeaderParamWalkNodes).
type headerParamWalker struct {
	seen      map[string]struct{}
	params    []HeaderParam
	nodeCount int
}

// ToolHeaderParams walks the ENTIRE schema document — not merely its
// top-level "properties" — for x-mcp-header annotations, and validates every
// one it finds against MCP 2026-07-28 §4.3. It returns the validated
// bindings in deterministic order (sorted by lowercased Name), or a nil
// slice and nil error if schema carries no such annotation at all.
//
// If ANY annotation anywhere in the document violates a constraint, the
// returned error wraps ErrHeaderParamConstraint and the slice is nil. The
// caller MUST then exclude the WHOLE tool from tools/list — not merely
// ignore the offending annotation and keep the rest — because §4.3 makes no
// provision for a partially-honored schema: "Verletzt eine Annotation die
// Constraints, MUSS der Client das betroffene Tool aus dem
// tools/list-Ergebnis ausschließen." FilterHeaderParamTools is the intended
// caller for exactly this reason; it never surfaces this error, it acts on
// it.
//
// A property is annotated on a node that is "statically reachable" when the
// chain of JSON Schema keywords from the document's root to that node
// consists ENTIRELY of "properties" keys. walk enforces this as an
// allowlist, not a blocklist: descending through a "properties" key is the
// ONLY way reachability survives a level. Descending through any other key —
// "items", "oneOf", "$ref", "$defs", "patternProperties",
// "dependentSchemas", "const", "contains", "propertyNames",
// "unevaluatedProperties", a vendor extension keyword, or anything else this
// package has no specific knowledge of — clears it. Once cleared, every
// descendant — however many "properties" levels further down — is
// permanently unreachable; the walk never recovers reachability once lost,
// matching §4.3's own phrasing ("nur auf Properties, die statisch erreichbar
// sind, also über eine reine Kette von properties-Keys"). An allowlist is
// deliberate here, not merely equivalent to a blocklist of the keywords §4.3
// happens to name: JSON Schema 2020-12 defines dozens of keywords the spec's
// examples do not enumerate, and a blocklist is only ever as complete as its
// last update — the one gap CANNOT recur under an allowlist, because nothing
// outside "properties" is ever asked to prove it belongs on the list of
// exceptions.
//
// An annotation sitting directly on the schema's own root — reached with no
// "properties" key anywhere on its path at all — is unreachable for the
// identical reason: an empty Path names no property for the annotation to
// describe, so recordAnnotation rejects it exactly as it would reject one
// found after crossing an "items" or "$ref" (see recordAnnotation's own
// doc).
//
// This applies even to an annotation sitting inside a "$defs"/"definitions"
// block that some OTHER part of the schema references via "$ref" and
// therefore effectively "uses": §4.3 conditions reachability on how a
// property is REACHED by the walk, not on whether it happens to be used
// anywhere. A server that annotates inside "$defs" is asking for a mirroring
// guarantee this package cannot make — the same schema keyword could be
// referenced from multiple places, or never resolved to a concrete
// tools/call argument path at all — so the annotation is rejected exactly as
// if it sat inside a genuinely dead branch.
func ToolHeaderParams(schema JSONSchema) ([]HeaderParam, error) {
	if len(schema) == 0 {
		return nil, nil
	}

	var doc any
	if err := jsonx.Unmarshal(schema, &doc); err != nil {
		// A document this package cannot even parse as JSON cannot be shown
		// to satisfy §4.3's constraints either — treated the same as a
		// constraint violation so the caller excludes the tool rather than
		// guessing at a partially-understood schema. err's own message is
		// deliberately never embedded here: this package's JSON decoder
		// (internal/jsonx, backed by sonic) reports a syntax error by
		// quoting a window of the SOURCE bytes around the failure position,
		// which would put upstream-controlled schema content — arbitrary
		// bytes from the tool definition, not merely its structure — into a
		// Warn log line via FilterHeaderParamTools. The wrapped sentinel
		// plus this message is diagnosis enough: this schema did not even
		// parse as JSON.
		return nil, fmt.Errorf("%w: schema is not valid JSON", ErrHeaderParamConstraint)
	}

	w := &headerParamWalker{seen: make(map[string]struct{})}
	if err := w.walk(doc, nil, true, 0); err != nil {
		return nil, err
	}
	if len(w.params) == 0 {
		return nil, nil
	}

	sort.Slice(w.params, func(i, j int) bool {
		return strings.ToLower(w.params[i].Name) < strings.ToLower(w.params[j].Name)
	})
	return w.params, nil
}

// walk visits node — one JSON value from the schema document — recording any
// x-mcp-header annotation found on it, then descends into node's children
// according to which JSON Schema keyword, if any, they sit under: extending
// path and preserving reachable for a "properties" child, clearing reachable
// (without extending path) for a child under EVERY other key. This is
// deliberately an allowlist of exactly one keyword rather than a blocklist of
// every keyword that must clear it — see ToolHeaderParams' own doc for why a
// blocklist could never enumerate the full JSON Schema 2020-12 vocabulary.
// The walk still visits children reached this way, so a violation nested
// under an unreachable branch is still found and still excludes the tool;
// they are simply never recorded as reachable.
//
// depth is degrees "properties"/composition-keyword nesting, not raw JSON
// object nesting — this docstring is not the place to legislate stack
// depth, only the meaningful nesting from a schema-authoring point of view.
func (w *headerParamWalker) walk(node any, path []string, reachable bool, depth int) error {
	w.nodeCount++
	if w.nodeCount > maxHeaderParamWalkNodes || depth > maxHeaderParamWalkDepth {
		return fmt.Errorf("%w: schema exceeds walk limits (max depth %d, max %d nodes)",
			ErrHeaderParamConstraint, maxHeaderParamWalkDepth, maxHeaderParamWalkNodes)
	}

	switch v := node.(type) {
	case map[string]any:
		_, hasRef := v["$ref"]
		childReachable := reachable && !hasRef

		if rawName, ok := v["x-mcp-header"]; ok {
			if err := w.recordAnnotation(v, rawName, path, reachable); err != nil {
				return err
			}
		}

		for key, val := range v {
			switch key {
			case "x-mcp-header":
				// Already handled above; the annotation value itself (a
				// string) has no children worth descending into.
			case "properties":
				props, ok := val.(map[string]any)
				if !ok {
					continue
				}
				for propName, propSchema := range props {
					childPath := append(append([]string(nil), path...), propName)
					if err := w.walk(propSchema, childPath, childReachable, depth+1); err != nil {
						return err
					}
				}
			default:
				// Every key other than "properties" clears reachability for
				// its children — see ToolHeaderParams' and this function's
				// own docs for why this is an allowlist of one keyword
				// rather than a blocklist of every keyword that must clear
				// it.
				if err := w.walk(val, path, false, depth+1); err != nil {
					return err
				}
			}
		}

	case []any:
		for _, item := range v {
			if err := w.walk(item, path, false, depth+1); err != nil {
				return err
			}
		}
	}

	return nil
}

// maxHeaderParamNameInError bounds how much of an x-mcp-header annotation
// name — upstream-controlled schema content — is embedded, via %q, into a
// diagnostic error message recordAnnotation produces. Those strings flow
// into a Warn log line (FilterHeaderParamTools), so an attacker-controlled
// schema property must never be able to inflate a log line without limit
// merely by choosing a long "x-mcp-header" value. The bound is applied
// unconditionally, at the top of recordAnnotation, rather than relying on
// MaxParamHeaderNameLength (the length constraint this package enforces on
// a VALID annotation) to do that job indirectly: a name can fail an earlier
// check — "not statically reachable", a duplicate, a bad "type" — without
// the MaxParamHeaderNameLength check ever running at all, so that check
// alone cannot bound every error text this function can produce; the
// assertion must not depend on which check happens to run, or in which
// order. Set well above MaxParamHeaderNameLength (64) so a valid name is
// always shown in full, and small enough that even a multi-megabyte schema
// property cannot turn one Warn line into an unbounded write.
const maxHeaderParamNameInError = 128

// truncateForError bounds s to at most maxHeaderParamNameInError bytes for
// embedding into a diagnostic error message, appending a truncation marker
// when s was cut. Byte truncation can split a multi-byte UTF-8 rune; that is
// acceptable here — the result is used only inside a %q-formatted
// diagnostic string, never re-parsed or compared, and %q already escapes
// whatever invalid UTF-8 the split produces.
func truncateForError(s string) string {
	if len(s) <= maxHeaderParamNameInError {
		return s
	}
	return s[:maxHeaderParamNameInError] + "...(truncated)"
}

// recordAnnotation validates a single x-mcp-header annotation found on node
// (the schema object it sits on, so "type" can be read alongside it),
// appends it to w.params on success, and returns an error wrapping
// ErrHeaderParamConstraint on any violation — see ToolHeaderParams' own doc
// for the full constraint list this enforces. Every error text below embeds
// safeName, never name itself — see maxHeaderParamNameInError's doc for why
// that bound is applied before any of these checks run, not only to the
// ones the length constraint would otherwise happen to gate.
func (w *headerParamWalker) recordAnnotation(node map[string]any, rawName any, path []string, reachable bool) error {
	name, ok := rawName.(string)
	if !ok || name == "" {
		return fmt.Errorf("%w: x-mcp-header value must be a non-empty JSON string", ErrHeaderParamConstraint)
	}
	safeName := truncateForError(name)
	if !reachable || len(path) == 0 {
		// len(path) == 0 means this annotation sits directly on the
		// schema's own root: no "properties" key ever led the walk here, so
		// there is no annotated PROPERTY for it to describe (see
		// ToolHeaderParams' own doc) — rejected the same way as any other
		// unreachable annotation.
		return fmt.Errorf("%w: x-mcp-header on %q is not statically reachable", ErrHeaderParamConstraint, safeName)
	}
	if len(name) > MaxParamHeaderNameLength || !IsHTTPToken(name) {
		return fmt.Errorf("%w: x-mcp-header value %q is not a valid HTTP token of at most %d bytes",
			ErrHeaderParamConstraint, safeName, MaxParamHeaderNameLength)
	}

	lower := strings.ToLower(name)
	if _, dup := w.seen[lower]; dup {
		return fmt.Errorf("%w: x-mcp-header name %q collides case-insensitively with another annotation in this schema",
			ErrHeaderParamConstraint, safeName)
	}

	var kind HeaderParamKind
	switch typ, _ := node["type"].(string); typ {
	case "string":
		kind = HeaderParamKindString
	case "integer":
		kind = HeaderParamKindInteger
	case "boolean":
		kind = HeaderParamKindBoolean
	default:
		// Covers a missing "type", "number" (deliberately excluded — see
		// HeaderParamKind's doc), any other primitive or composite type, and
		// the JSON-Schema array form ["string","null"] — a type assertion to
		// string fails for a non-string JSON value, landing here too.
		return fmt.Errorf("%w: x-mcp-header property %q must declare type exactly one of string/integer/boolean",
			ErrHeaderParamConstraint, safeName)
	}

	w.seen[lower] = struct{}{}
	w.params = append(w.params, HeaderParam{
		Name: name,
		Path: append([]string(nil), path...),
		Kind: kind,
	})
	if len(w.params) > MaxParamHeaders {
		return fmt.Errorf("%w: schema declares more than %d x-mcp-header annotations",
			ErrHeaderParamConstraint, MaxParamHeaders)
	}
	return nil
}

// FilterHeaderParamTools returns the subset of tools whose input schema's
// x-mcp-header annotations, if any, all satisfy MCP 2026-07-28 §4.3
// (ToolHeaderParams), plus a map from each KEPT tool's name to its validated
// bindings. A tool absent from params either declared no annotation at all,
// or is itself absent from kept.
//
// A tool whose schema fails ToolHeaderParams is excluded from kept in its
// entirety — never partially, and never with the annotation merely dropped —
// and logged once, at Warn, with the server ID, tool name, and the
// constraint-violation reason ToolHeaderParams returned. That reason
// describes schema STRUCTURE (a property name, a missing "type" keyword,
// a nesting depth) — never an argument value, because no tools/call
// argument exists yet at tools/list time for there to be one to log.
// ctx is used only to derive the logger's context (trace correlation, log
// level); no I/O is performed here — tools/list has already returned by the
// time this runs.
func FilterHeaderParamTools(ctx context.Context, serverID string, tools []Tool) (kept []Tool, params map[string][]HeaderParam) {
	if len(tools) == 0 {
		return tools, nil
	}

	kept = make([]Tool, 0, len(tools))
	for _, tool := range tools {
		hp, err := ToolHeaderParams(tool.InputSchema)
		if err != nil {
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "mcp: tool excluded from tools/list: x-mcp-header constraint violation",
				slog.String("server_id", serverID),
				slog.String("tool_name", tool.Name),
				slog.String("reason", err.Error()),
			)
			continue
		}
		kept = append(kept, tool)
		if len(hp) > 0 {
			if params == nil {
				params = make(map[string][]HeaderParam, len(tools))
			}
			params[tool.Name] = hp
		}
	}
	return kept, params
}

// headerParamValue resolves p against arguments — the raw JSON value of a
// tools/call request's params.arguments — walking p.Path level by level to
// the annotated leaf and rendering it per MCP 2026-07-28 §4.4's requirement
// that the header and body values compare equal, byte-for-byte, under
// whichever comparison (string or numeric) the receiving server applies.
//
//   - HeaderParamKindString: the leaf must be a JSON string; it is
//     unmarshalled (reversing JSON string escaping) and the decoded Go string
//     is returned.
//   - HeaderParamKindInteger: the leaf must be a JSON number token containing
//     no '.', 'e', or 'E' — i.e. a JSON integer literal, not merely a number
//     that happens to have an integral value. The token is returned VERBATIM,
//     exactly as it appears in arguments: never round-tripped through
//     float64 (which loses precision for a large int64) and never
//     re-rendered. Verbatim is what guarantees the header and the body are
//     byte-identical, which is what makes them agree under both a string
//     comparison and §4.5's recommended numeric one.
//   - HeaderParamKindBoolean: the leaf must be the literal token "true" or
//     "false"; it is returned verbatim.
//
// Any other shape — the path does not resolve (a missing key, or an
// intermediate value that is not a JSON object), the leaf is JSON null, or
// the leaf's JSON type does not match Kind — reports ok == false and no
// error. This is deliberate, not a shortcut: a mismatch between the schema's
// declared type and the argument actually sent means the upstream will
// reject the call against its own schema regardless of what VoidLLM does
// here: sending a wrongly typed header alongside that already-doomed body
// would only turn a clean tool-level rejection into a confusing §4.5
// HeaderMismatch (-32020) on top of it.
func headerParamValue(arguments jsonx.RawMessage, p HeaderParam) (string, bool) {
	if len(arguments) == 0 || len(p.Path) == 0 {
		return "", false
	}

	cur := arguments
	for _, key := range p.Path {
		var obj map[string]jsonx.RawMessage
		if err := jsonx.Unmarshal(cur, &obj); err != nil {
			return "", false
		}
		val, ok := obj[key]
		if !ok {
			return "", false
		}
		cur = val
	}

	return renderHeaderParamLeaf(cur, p.Kind)
}

// renderHeaderParamLeaf implements the per-Kind rendering rules documented
// on headerParamValue, given the raw JSON value found at the end of a
// HeaderParam's Path.
func renderHeaderParamLeaf(raw jsonx.RawMessage, kind HeaderParamKind) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}

	switch kind {
	case HeaderParamKindString:
		if trimmed[0] != '"' {
			return "", false
		}
		var s string
		if err := jsonx.Unmarshal([]byte(trimmed), &s); err != nil {
			return "", false
		}
		return s, true
	case HeaderParamKindInteger:
		if !isJSONBareInteger(trimmed) {
			return "", false
		}
		return trimmed, true
	case HeaderParamKindBoolean:
		if trimmed == "true" || trimmed == "false" {
			return trimmed, true
		}
		return "", false
	default:
		return "", false
	}
}

// isJSONBareInteger reports whether s is a JSON number token with no
// fractional or exponent part — an optional leading '-' followed by one or
// more ASCII digits, and nothing else. s is assumed to already be a
// syntactically valid JSON token (it was extracted by unmarshalling a larger
// JSON document that itself parsed successfully), so this only needs to
// distinguish an integer literal from one carrying '.', 'e', or 'E' — it does
// not re-validate JSON number grammar (leading-zero rules, digit presence)
// from scratch.
func isJSONBareInteger(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
