package mcp_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- ToolHeaderParams: one isolated §4.3 constraint per row ----------------

// TestToolHeaderParams_ConstraintTable drives ToolHeaderParams over a schema
// that violates exactly one MCP 2026-07-28 §4.3 constraint per case — see
// each case's own comment for which rule it isolates and why the schema is
// shaped the way it is (in particular the "behind a $ref" and "under $defs"
// cases, which look similar but exercise different branches of the walker:
// $defs rejects purely because the $defs keyword itself clears reachability,
// with no $ref anywhere in the schema; "behind a $ref" rejects because the
// path to the annotation crosses a node that itself carries "$ref" as a
// sibling keyword, before any $defs involvement).
//
// Every case must report errors.Is(err, mcp.ErrHeaderParamConstraint). This
// test goes red the moment any one of these individual rules is loosened —
// e.g. IsHTTPToken accepting '@', the case-insensitive duplicate check being
// dropped, "number" being added to the allowed kind switch, or walk's
// reachability check stopping being an allowlist of "properties" alone (were
// it ever to regress to a blocklist of named keywords, the
// "dependentSchemas" and "schema root" cases below are what would catch a
// keyword the blocklist forgot to name) — because each case is built to
// depend on exactly one rule holding.
func TestToolHeaderParams_ConstraintTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema string
	}{
		{
			name:   "empty x-mcp-header name",
			schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":""}}}`,
		},
		{
			name:   "@ is not a valid HTTP token character",
			schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Reg@ion"}}}`,
		},
		{
			name:   "a control character (tab) in the name",
			schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Reg\tion"}}}`,
		},
		{
			name:   "annotation value is a JSON number, not a string",
			schema: `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":123}}}`,
		},
		{
			name:   "two annotations collide case-insensitively (Region vs region)",
			schema: `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Region"},"b":{"type":"string","x-mcp-header":"region"}}}`,
		},
		{
			name:   `type: "number" is a JSON Schema primitive but not an allowed HeaderParamKind`,
			schema: `{"type":"object","properties":{"amount":{"type":"number","x-mcp-header":"Amount"}}}`,
		},
		{
			name:   "type keyword absent entirely",
			schema: `{"type":"object","properties":{"region":{"x-mcp-header":"Region"}}}`,
		},
		{
			name:   `type is the JSON Schema array form ["string","null"]`,
			schema: `{"type":"object","properties":{"region":{"type":["string","null"],"x-mcp-header":"Region"}}}`,
		},
		{
			name:   "annotation sits under items",
			schema: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string","x-mcp-header":"Tag"}}}}`,
		},
		{
			name:   "annotation sits under oneOf[0]",
			schema: `{"type":"object","oneOf":[{"type":"string","x-mcp-header":"Foo"}]}`,
		},
		{
			name:   "annotation sits under if",
			schema: `{"type":"object","if":{"properties":{"foo":{"type":"string","x-mcp-header":"Foo"}}}}`,
		},
		{
			name:   "annotation sits directly under $defs, never referenced by any $ref",
			schema: `{"type":"object","$defs":{"Region":{"type":"string","x-mcp-header":"Region"}}}`,
		},
		{
			name: "annotation is reached only by first crossing a node that carries $ref",
			schema: `{"type":"object","properties":{"region":{"$ref":"#/$defs/Base",` +
				`"properties":{"code":{"type":"string","x-mcp-header":"Region"}}}}}`,
		},
		{
			// Distinct from the case immediately above: there the $ref sits
			// on an ANCESTOR of the annotated node (region), clearing
			// reachability for its children before the walk ever reaches the
			// annotation. Here $ref and x-mcp-header are both properties of
			// the very SAME node — the annotation is not reached "through" a
			// $ref-carrying ancestor at all, it sits directly ON one. This
			// exercises walk's own node-vs-children distinction: hasRef
			// clears childReachable for this node's CHILDREN, but the node's
			// OWN annotation must use that same childReachable, not the
			// unadjusted (still-reachable) value the node itself was visited
			// with — otherwise a schema that pairs $ref with a local "type"
			// (whose truthfulness this package cannot verify against
			// whatever the $ref actually resolves to) would sail through as
			// reachable.
			name:   "$ref sits on the SAME node as the x-mcp-header annotation, not on an ancestor",
			schema: `{"type":"object","properties":{"region":{"type":"string","$ref":"#/$defs/Base","x-mcp-header":"Region"}}}`,
		},
		{
			// Regression case for the walk's reachability check being an
			// allowlist of "properties" alone, not a blocklist of named
			// composition keywords: "dependentSchemas" is not, and has never
			// been, one of the keywords §4.3's own text enumerates
			// ("items", "oneOf"/"anyOf"/"allOf"/"not", "if"/"then"/"else",
			// "$ref"). A blocklist limited to that literal list would wrongly
			// treat this annotation as reachable; the allowlist rejects it
			// exactly the same as every other non-"properties" keyword.
			name:   "annotation sits under a composition keyword §4.3's own text never names (dependentSchemas)",
			schema: `{"type":"object","dependentSchemas":{"foo":{"properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}}`,
		},
		{
			// An annotation directly on the schema's own root, with no
			// "properties" key anywhere on the path to it, has no annotated
			// PROPERTY at all — an empty Path names nothing, so
			// recordAnnotation rejects it the same way as any other
			// unreachable annotation.
			name:   "annotation sits directly on the schema root, not inside any property",
			schema: `{"type":"object","x-mcp-header":"Region","properties":{"q":{"type":"string"}}}`,
		},
		{
			name:   "17 individually valid annotations exceed MaxParamHeaders (16)",
			schema: manyHeaderParamSchema(t, 17),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			params, err := mcp.ToolHeaderParams(mcp.JSONSchema(tc.schema))
			if err == nil {
				t.Fatalf("ToolHeaderParams() error = nil, want a wrapped ErrHeaderParamConstraint; params = %+v", params)
			}
			if !errors.Is(err, mcp.ErrHeaderParamConstraint) {
				t.Errorf("ToolHeaderParams() error = %v, want it to wrap ErrHeaderParamConstraint", err)
			}
			if params != nil {
				t.Errorf("ToolHeaderParams() params = %+v, want nil on any constraint violation", params)
			}
		})
	}
}

// manyHeaderParamSchema builds an object schema with n top-level properties,
// each individually a valid x-mcp-header annotation (a distinct, HTTP-token,
// case-insensitively-unique name on a "string"-typed property) — used to
// isolate the MaxParamHeaders count cap from every per-annotation rule,
// which n=17 (one over the cap of 16) alone must trigger.
func manyHeaderParamSchema(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"prop%d":{"type":"string","x-mcp-header":"Header%d"}`, i, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// ---- ToolHeaderParams: happy path -------------------------------------------

// TestToolHeaderParams_ValidSchema_NoError is the Gegenprobe for the
// constraint table: a schema whose annotations satisfy every §4.3 rule
// returns a non-nil, error-free result with the expected Kind for each of
// the three allowed primitive types.
func TestToolHeaderParams_ValidSchema_NoError(t *testing.T) {
	t.Parallel()

	schema := mcp.JSONSchema(`{
		"type": "object",
		"properties": {
			"region": {"type": "string", "x-mcp-header": "Region"},
			"limit":  {"type": "integer", "x-mcp-header": "Limit"},
			"dryRun": {"type": "boolean", "x-mcp-header": "Dry-Run"},
			"query":  {"type": "string"}
		}
	}`)

	params, err := mcp.ToolHeaderParams(schema)
	if err != nil {
		t.Fatalf("ToolHeaderParams() error = %v, want nil", err)
	}
	if len(params) != 3 {
		t.Fatalf("len(params) = %d, want 3 (query carries no annotation)", len(params))
	}

	byName := make(map[string]mcp.HeaderParam, len(params))
	for _, p := range params {
		byName[p.Name] = p
	}
	if p, ok := byName["Region"]; !ok || p.Kind != mcp.HeaderParamKindString {
		t.Errorf("Region binding = %+v, ok=%v, want Kind=String", p, ok)
	}
	if p, ok := byName["Limit"]; !ok || p.Kind != mcp.HeaderParamKindInteger {
		t.Errorf("Limit binding = %+v, ok=%v, want Kind=Integer", p, ok)
	}
	if p, ok := byName["Dry-Run"]; !ok || p.Kind != mcp.HeaderParamKindBoolean {
		t.Errorf("Dry-Run binding = %+v, ok=%v, want Kind=Boolean", p, ok)
	}
}

// TestToolHeaderParams_NoAnnotations_NilNilResult verifies an unannotated
// schema is not itself a violation: nil slice, nil error.
func TestToolHeaderParams_NoAnnotations_NilNilResult(t *testing.T) {
	t.Parallel()

	params, err := mcp.ToolHeaderParams(mcp.JSONSchema(`{"type":"object","properties":{"q":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("ToolHeaderParams() error = %v, want nil", err)
	}
	if params != nil {
		t.Errorf("ToolHeaderParams() params = %+v, want nil", params)
	}
}

// TestToolHeaderParams_NestedPath verifies a statically reachable annotation
// nested two "properties" levels deep is found with Path == ["outer",
// "region"], not merely detected at the top level — proving the walk
// actually threads the accumulated path through nested objects rather than
// only checking direct children of the schema root. Rot if Path were built
// from only the immediate parent key, or if nested objects were skipped
// entirely.
func TestToolHeaderParams_NestedPath(t *testing.T) {
	t.Parallel()

	schema := mcp.JSONSchema(`{
		"type": "object",
		"properties": {
			"outer": {
				"type": "object",
				"properties": {
					"region": {"type": "string", "x-mcp-header": "Region"}
				}
			}
		}
	}`)

	params, err := mcp.ToolHeaderParams(schema)
	if err != nil {
		t.Fatalf("ToolHeaderParams() error = %v, want nil", err)
	}
	if len(params) != 1 {
		t.Fatalf("len(params) = %d, want 1", len(params))
	}
	got := params[0].Path
	want := []string{"outer", "region"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Path = %v, want %v", got, want)
	}
}

// ---- ToolHeaderParams: walk limits terminate, not merely error -------------

// deepNestedSchema builds an object schema n "properties" levels deep, with
// an x-mcp-header annotation on the innermost property.
func deepNestedSchema(n int) string {
	var open, close string
	for i := 0; i < n; i++ {
		open += fmt.Sprintf(`{"type":"object","properties":{"level%d":`, i)
		close += `}}`
	}
	return open + `{"type":"string","x-mcp-header":"Deep"}` + close
}

// wideSchema builds a single-level object schema with n sibling properties,
// none annotated, to exercise maxHeaderParamWalkNodes independent of depth.
func wideSchema(n int) string {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"p%d":{"type":"string"}`, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// TestToolHeaderParams_WalkLimits_TerminateQuickly verifies that a schema
// exceeding maxHeaderParamWalkDepth or maxHeaderParamWalkNodes is rejected
// (wrapping ErrHeaderParamConstraint, per ToolHeaderParams' documented
// pessimistic-by-design behavior) AND that ToolHeaderParams actually returns
// in well under a second either way. The assertion that matters here is
// termination, not merely "returns an error" — a walk with a broken depth or
// node-count check could still error eventually (e.g. via Go's own stack
// overflow or an OOM) after a very long time; measuring the wall-clock
// duration is what catches that, a bare error check would not.
func TestToolHeaderParams_WalkLimits_TerminateQuickly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema string
	}{
		{name: "20 levels deep, exceeds maxHeaderParamWalkDepth", schema: deepNestedSchema(20)},
		{name: "5000 sibling properties, exceeds maxHeaderParamWalkNodes", schema: wideSchema(5000)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			start := time.Now()
			params, err := mcp.ToolHeaderParams(mcp.JSONSchema(tc.schema))
			elapsed := time.Since(start)

			if elapsed > time.Second {
				t.Errorf("ToolHeaderParams() took %s, want well under 1s (walk limits must bound the work, not merely eventually error)", elapsed)
			}
			if err == nil {
				t.Fatalf("ToolHeaderParams() error = nil, params = %+v, want a walk-limit error", params)
			}
			if !errors.Is(err, mcp.ErrHeaderParamConstraint) {
				t.Errorf("ToolHeaderParams() error = %v, want it to wrap ErrHeaderParamConstraint", err)
			}
		})
	}
}

// TestToolHeaderParams_WithinLimits_Succeeds is the Gegenprobe for the walk
// limits: a schema comfortably inside both bounds (well under
// maxHeaderParamWalkDepth and maxHeaderParamWalkNodes) is accepted, not
// rejected — proving the limits are not so tight that ordinary schemas trip
// them.
func TestToolHeaderParams_WithinLimits_Succeeds(t *testing.T) {
	t.Parallel()

	params, err := mcp.ToolHeaderParams(mcp.JSONSchema(deepNestedSchema(5)))
	if err != nil {
		t.Fatalf("ToolHeaderParams() error = %v, want nil for a schema well within the walk limits", err)
	}
	if len(params) != 1 || params[0].Name != "Deep" {
		t.Errorf("params = %+v, want a single Deep binding", params)
	}
}

// ---- FilterHeaderParamTools -------------------------------------------------

// TestFilterHeaderParamTools_ExcludesOnlyViolatingTool verifies MCP
// 2026-07-28 §4.3's exact remedy for a tool whose schema violates a
// constraint: that ONE tool is excluded from the returned list, and the
// other two — including their InputSchema and Description — are returned
// completely unchanged. This is falsifiable in both directions: dropping the
// whole list on any single violation (returning kept == nil) fails the
// length assertion, and keeping the broken tool while merely dropping its
// annotation (returning all three tools) also fails the length assertion —
// only excluding exactly the one broken tool satisfies both checks at once.
func TestFilterHeaderParamTools_ExcludesOnlyViolatingTool(t *testing.T) {
	t.Parallel()

	goodTool := mcp.Tool{
		Name:        "lookup_region",
		Description: "Looks up a region",
		InputSchema: mcp.JSONSchema(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`),
	}
	plainTool := mcp.Tool{
		Name:        "no_annotation_tool",
		Description: "Carries no x-mcp-header at all",
		InputSchema: mcp.JSONSchema(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
	brokenTool := mcp.Tool{
		Name:        "broken_tool",
		Description: "Violates §4.3: annotation under oneOf",
		InputSchema: mcp.JSONSchema(`{"type":"object","oneOf":[{"type":"string","x-mcp-header":"Foo"}]}`),
	}

	kept, params := mcp.FilterHeaderParamTools(t.Context(), "server-1", []mcp.Tool{goodTool, plainTool, brokenTool})

	if len(kept) != 2 {
		t.Fatalf("len(kept) = %d, want 2 (exactly the two non-violating tools); kept = %+v", len(kept), kept)
	}
	byName := make(map[string]mcp.Tool, len(kept))
	for _, tl := range kept {
		byName[tl.Name] = tl
	}
	if _, ok := byName["broken_tool"]; ok {
		t.Error("kept contains broken_tool, want it excluded")
	}
	got, ok := byName["lookup_region"]
	if !ok {
		t.Fatal("kept is missing lookup_region")
	}
	if got.Description != goodTool.Description || string(got.InputSchema) != string(goodTool.InputSchema) {
		t.Errorf("lookup_region = %+v, want unchanged from input %+v", got, goodTool)
	}
	if _, ok := byName["no_annotation_tool"]; !ok {
		t.Error("kept is missing no_annotation_tool")
	}

	if len(params) != 1 {
		t.Fatalf("len(params) = %d, want 1 (only lookup_region carries a binding)", len(params))
	}
	if _, ok := params["lookup_region"]; !ok {
		t.Errorf("params = %+v, want a \"lookup_region\" entry", params)
	}
	if _, ok := params["broken_tool"]; ok {
		t.Error("params contains an entry for the excluded broken_tool")
	}
}

// TestFilterHeaderParamTools_EmptyInput verifies the degenerate empty-slice
// case does not panic and returns the input unchanged.
func TestFilterHeaderParamTools_EmptyInput(t *testing.T) {
	t.Parallel()

	kept, params := mcp.FilterHeaderParamTools(t.Context(), "server-1", nil)
	if kept != nil {
		t.Errorf("kept = %+v, want nil", kept)
	}
	if params != nil {
		t.Errorf("params = %+v, want nil", params)
	}
}
