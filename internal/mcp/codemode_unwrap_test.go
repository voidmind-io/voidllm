package mcp_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

func TestUnwrapToolResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       jsonx.RawMessage
		wantRaw     bool // true = expect byte-identical passthrough of input
		wantPayload jsonx.RawMessage
	}{
		{
			name:        "SingleTextBlockWithJSON",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"{\"key\":\"value\"}"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`{"key":"value"}`),
		},
		{
			name:        "SingleTextBlockWithPlainText",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"hello world"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`"hello world"`),
		},
		{
			name:        "SingleTextBlockEmptyString",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":""}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`""`),
		},
		{
			name:        "MultipleTextBlocks",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`["first","second"]`),
		},
		{
			name:        "MultipleMixedBlocks_TextAndNonText",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"a"},{"type":"image","text":""},{"type":"text","text":"b"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`["a","b"]`),
		},
		{
			name:    "MultipleNonTextBlocks_PassThrough",
			input:   jsonx.RawMessage(`{"content":[{"type":"image","text":""},{"type":"image","text":""}]}`),
			wantRaw: true,
		},
		{
			name:    "SingleNonTextBlock_PassThrough",
			input:   jsonx.RawMessage(`{"content":[{"type":"image","text":""}]}`),
			wantRaw: true,
		},
		{
			name:    "NotAToolResult_NoContentKey",
			input:   jsonx.RawMessage(`{"score":99}`),
			wantRaw: true,
		},
		{
			name:    "UnknownTopLevelKey_PassThrough",
			input:   jsonx.RawMessage(`{"content":[{"type":"text","text":"x"}],"userFoo":"bar"}`),
			wantRaw: true,
		},
		{
			name:        "AllowedTopLevelKeys_StillUnwraps",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"x"}],"isError":false,"_meta":{},"structuredContent":null}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`"x"`),
		},
		{
			name:    "EmptyContentArray_PassThrough",
			input:   jsonx.RawMessage(`{"content":[]}`),
			wantRaw: true,
		},
		{
			name:    "InvalidJSON_PassThrough",
			input:   jsonx.RawMessage(`not json at all`),
			wantRaw: true,
		},
		{
			name:    "ContentKeyWrongType_PassThrough",
			input:   jsonx.RawMessage(`{"content":"not an array"}`),
			wantRaw: true,
		},
		{
			name:        "NestedJSONInText_Array",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"[1,2,3]"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`[1,2,3]`),
		},
		{
			name:    "UppercaseContentKey_PassThrough",
			input:   jsonx.RawMessage(`{"Content":[{"type":"text","text":"x"}]}`),
			wantRaw: true,
		},
		{
			// inner content is valid JSON — returns unquoted; contrast with SingleTextBlockWithPlainText
			name:        "SingleTextBlockWithJSONNumber",
			input:       jsonx.RawMessage(`{"content":[{"type":"text","text":"42"}]}`),
			wantRaw:     false,
			wantPayload: jsonx.RawMessage(`42`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.UnwrapToolResult(tc.input)

			if tc.wantRaw {
				if !bytes.Equal(got, tc.input) {
					t.Errorf("case %q: expected raw passthrough\n  input: %s\n  got:   %s",
						tc.name, tc.input, got)
				}
				return
			}

			// Compare as parsed JSON to tolerate key-ordering differences.
			var gotParsed, wantParsed interface{}
			if err := jsonx.Unmarshal(got, &gotParsed); err != nil {
				t.Fatalf("case %q: got is not valid JSON: %v\n  got: %s", tc.name, err, got)
			}
			if err := jsonx.Unmarshal(tc.wantPayload, &wantParsed); err != nil {
				t.Fatalf("case %q: wantPayload is not valid JSON: %v\n  wantPayload: %s", tc.name, err, tc.wantPayload)
			}
			if !reflect.DeepEqual(gotParsed, wantParsed) {
				t.Errorf("case %q: payload mismatch\n  input: %s\n  got:   %s\n  want:  %s",
					tc.name, tc.input, got, tc.wantPayload)
			}
		})
	}
}
