package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// bsFeed feeds payloads (one event JSON each, "data: " prefix added) followed
// by a blank line into a, returning all output lines and the first error.
func bsFeed(a *BedrockAdapter, payloads ...string) ([]string, error) {
	var lines []string
	for _, p := range payloads {
		out, err := a.TransformStreamLine([]byte("data: " + p))
		for _, l := range out {
			lines = append(lines, string(l))
		}
		if err != nil {
			return lines, err
		}
		blank, err := a.TransformStreamLine(nil)
		if err != nil {
			return lines, err
		}
		for _, l := range blank {
			lines = append(lines, string(l))
		}
	}
	return lines, nil
}

// bsChunks decodes every "data: {json}" line into a chunk map.
func bsChunks(t *testing.T, lines []string) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	for _, l := range lines {
		if !strings.HasPrefix(l, "data: ") || l == "data: [DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &m); err != nil {
			t.Fatalf("line %q is not a JSON chunk: %v", l, err)
		}
		chunks = append(chunks, m)
	}
	return chunks
}

func bsDelta(c map[string]any) map[string]any {
	ch := c["choices"].([]any)
	if len(ch) == 0 {
		return nil
	}
	return ch[0].(map[string]any)["delta"].(map[string]any)
}

func bsFinish(c map[string]any) any {
	ch := c["choices"].([]any)
	if len(ch) == 0 {
		return nil
	}
	return ch[0].(map[string]any)["finish_reason"]
}

const (
	bsStart   = `{"messageStart":{"role":"assistant"}}`
	bsStop    = `{"messageStop":{"stopReason":"end_turn"}}`
	bsMeta    = `{"metadata":{"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15},"metrics":{"latencyMs":3}}}`
	bsBlkStop = `{"contentBlockStop":{"contentBlockIndex":0}}`
)

func bsText(idx int, text string) string {
	b, _ := json.Marshal(text)
	return fmt.Sprintf(`{"contentBlockDelta":{"contentBlockIndex":%d,"delta":{"text":%s}}}`, idx, b)
}

func bsToolStart(idx int, id, name string) string {
	return fmt.Sprintf(`{"contentBlockStart":{"contentBlockIndex":%d,"start":{"toolUse":{"toolUseId":%q,"name":%q}}}}`, idx, id, name)
}

func bsToolDelta(idx int, input string) string {
	b, _ := json.Marshal(input)
	return fmt.Sprintf(`{"contentBlockDelta":{"contentBlockIndex":%d,"delta":{"toolUse":{"input":%s}}}}`, idx, b)
}

func TestBedrockAdapter_StreamFullTextSequence(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{responseID: "chatcmpl-9", modelName: "m"}
	lines, err := bsFeed(a, bsStart, bsText(0, "Hel"), bsText(0, "lo"), bsBlkStop, bsStop, bsMeta)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	chunks := bsChunks(t, lines)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4 (role, 2x text, finish) without include_usage:\n%v", len(chunks), lines)
	}
	if bsDelta(chunks[0])["role"] != "assistant" {
		t.Errorf("first chunk = %v", chunks[0])
	}
	if bsDelta(chunks[1])["content"] != "Hel" || bsDelta(chunks[2])["content"] != "lo" {
		t.Errorf("text chunks = %v %v", chunks[1], chunks[2])
	}
	if bsFinish(chunks[3]) != "stop" {
		t.Errorf("finish chunk = %v", chunks[3])
	}
	for i, c := range chunks {
		if c["id"] != "chatcmpl-9" || c["object"] != "chat.completion.chunk" {
			t.Errorf("chunk %d envelope = %v", i, c)
		}
		if _, ok := c["usage"]; ok {
			t.Errorf("chunk %d carries usage without include_usage", i)
		}
	}
	if lines[len(lines)-2] != "data: [DONE]" || lines[len(lines)-1] != "" {
		t.Errorf("tail = %q, want [DONE] then blank", lines[len(lines)-2:])
	}
	n := 0
	for _, l := range lines {
		if l == "data: [DONE]" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("[DONE] count = %d, want 1", n)
	}
	if u := a.StreamUsage(); u.PromptTokens != 10 || u.CompletionTokens != 5 || u.TotalTokens != 15 {
		t.Errorf("StreamUsage = %+v", u)
	}
}

func TestBedrockAdapter_StreamIncludeUsageChunk(t *testing.T) {
	t.Parallel()

	meta := `{"metadata":{"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":20}}}`
	for name, order := range map[string][]string{
		"stop then metadata": {bsStart, bsText(0, "x"), bsStop, meta},
		"metadata then stop": {bsStart, bsText(0, "x"), meta, bsStop},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{includeUsage: true, responseID: "chatcmpl-1", modelName: "mm"}
			lines, err := bsFeed(a, order...)
			if err != nil {
				t.Fatal(err)
			}
			chunks := bsChunks(t, lines)
			last := chunks[len(chunks)-1]
			if len(last["choices"].([]any)) != 0 {
				t.Errorf("usage chunk choices = %v, want empty array", last["choices"])
			}
			u := last["usage"].(map[string]any)
			if u["prompt_tokens"] != float64(130) || u["completion_tokens"] != float64(5) || u["total_tokens"] != float64(135) {
				t.Errorf("usage = %v", u)
			}
			d := u["prompt_tokens_details"].(map[string]any)
			if d["cached_tokens"] != float64(100) || d["cache_creation_tokens"] != float64(20) {
				t.Errorf("details = %v", d)
			}
			if last["model"] != "mm" {
				t.Errorf("usage chunk model = %v", last["model"])
			}
			// The finish chunk must come before the usage chunk and carry no usage.
			var finishIdx = -1
			for i, c := range chunks {
				if bsFinish(c) == "stop" {
					finishIdx = i
				}
			}
			if finishIdx < 0 || finishIdx >= len(chunks)-1 {
				t.Errorf("finish chunk index %d of %d", finishIdx, len(chunks))
			}
			if lines[len(lines)-2] != "data: [DONE]" {
				t.Errorf("DONE must follow usage chunk, tail %q", lines[len(lines)-2:])
			}
			// Nothing at all may follow [DONE] except its blank separator.
			for i, l := range lines {
				if l == "data: [DONE]" && i < len(lines)-2 {
					t.Errorf("content after [DONE]: %q", lines[i:])
				}
			}
		})
	}
}

func TestBedrockAdapter_StreamMetadataBeforeStopNoEarlyDone(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	lines, err := bsFeed(a, bsStart, bsMeta)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l == "data: [DONE]" {
			t.Fatal("[DONE] emitted before messageStop")
		}
	}
	lines, err = bsFeed(a, bsStop)
	if err != nil {
		t.Fatal(err)
	}
	if lines[len(lines)-2] != "data: [DONE]" {
		t.Errorf("tail = %q", lines)
	}
}

func TestBedrockAdapter_StreamStopWithoutMetadataNoDone(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	lines, err := bsFeed(a, bsStart, bsText(0, "x"), bsStop)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l == "data: [DONE]" {
			t.Fatal("[DONE] must wait for metadata so usage is not lost")
		}
	}
}

func TestBedrockAdapter_StreamStopReasons(t *testing.T) {
	t.Parallel()
	for reason, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "model_context_window_exceeded": "length",
		"tool_use": "tool_calls", "guardrail_intervened": "content_filter", "content_filtered": "content_filter", "weird": "stop",
	} {
		a := &BedrockAdapter{}
		lines, err := bsFeed(a, bsStart, fmt.Sprintf(`{"messageStop":{"stopReason":%q}}`, reason), bsMeta)
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		var got any
		for _, c := range bsChunks(t, lines) {
			if f := bsFinish(c); f != nil {
				got = f
			}
		}
		if got != want {
			t.Errorf("stopReason %q: finish_reason = %v, want %q", reason, got, want)
		}
	}
}

func TestBedrockAdapter_StreamToolCalls(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{responseID: "chatcmpl-1"}
	stop := `{"messageStop":{"stopReason":"tool_use"}}`
	lines, err := bsFeed(a,
		bsStart,
		bsText(0, "thinking"),
		bsToolStart(1, "t-1", "get_weather"),
		bsToolDelta(1, `{"ci`),
		bsToolDelta(1, `ty":"Berlin"}`),
		`{"contentBlockStop":{"contentBlockIndex":1}}`,
		bsToolStart(2, "t_2", "other"),
		bsToolDelta(2, `{}`),
		stop, bsMeta,
	)
	if err != nil {
		t.Fatal(err)
	}
	chunks := bsChunks(t, lines)

	var headers []map[string]any
	var args = map[float64]string{}
	for _, c := range chunks {
		d := bsDelta(c)
		if d == nil {
			continue
		}
		tcs, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		tc := tcs[0].(map[string]any)
		idx := tc["index"].(float64)
		fn := tc["function"].(map[string]any)
		if _, hasID := tc["id"]; hasID {
			headers = append(headers, tc)
			if fn["arguments"] != "" {
				t.Errorf("header delta arguments = %v, want empty string", fn["arguments"])
			}
		} else if _, hasName := fn["name"]; hasName {
			t.Errorf("argument delta carries name: %v", tc)
		}
		args[idx] += fn["arguments"].(string)
	}
	if len(headers) != 2 {
		t.Fatalf("headers = %v", headers)
	}
	h0, h1 := headers[0], headers[1]
	if h0["id"] != "t-1" || h0["type"] != "function" || h0["index"] != float64(0) || h0["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("header 0 = %v", h0)
	}
	if h1["id"] != "t_2" || h1["index"] != float64(1) {
		t.Errorf("header 1 = %v (OpenAI index must be sequential, not the Bedrock block index)", h1)
	}
	if args[0] != `{"city":"Berlin"}` || args[1] != `{}` {
		t.Errorf("assembled args = %v", args)
	}
	var finish any
	for _, c := range chunks {
		if f := bsFinish(c); f != nil {
			finish = f
		}
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %v", finish)
	}
}

func TestBedrockAdapter_StreamAbortMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup []string
		line  string
	}{
		{"exception", nil, `{":exception":"throttlingException"}`},
		{"exception after content", []string{bsStart, bsText(0, "x")}, `{":exception":"internalServerException"}`},
		{"malformed json", nil, `{"messageStart":`},
		{"not an object", nil, `[1,2]`},
		{"string payload", nil, `"x"`},
		{"event member wrong type", nil, `{"contentBlockDelta":"x"}`},
		{"second messageStop", []string{bsStart, bsStop}, bsStop},
		{"second metadata", []string{bsStart, bsMeta}, bsMeta},
		{"negative tool index", nil, bsToolStart(-1, "a", "f")},
		{"duplicate tool index", []string{bsToolStart(1, "a", "f")}, bsToolStart(1, "b", "g")},
		{"unknown tool delta index", []string{bsToolStart(1, "a", "f")}, bsToolDelta(7, "x")},
		{"tool delta without start", nil, bsToolDelta(0, "x")},
		{"invalid tool id", nil, bsToolStart(0, "a b", "f")},
		{"empty tool id", nil, bsToolStart(0, "", "f")},
		{"invalid tool name", nil, bsToolStart(0, "a", "f.g")},
		{"empty tool name", nil, bsToolStart(0, "a", "")},
		{"too long tool id", nil, bsToolStart(0, strings.Repeat("a", 65), "f")},
		{"pseudonym shaped tool id", nil, bsToolStart(0, "PII_EM_0123456789abcdef01234567", "f")},
		{"pseudonym shaped tool name", nil, bsToolStart(0, "a", "PII_EM_0123456789abcdef01234567")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{}
			setup := tc.setup
			if len(setup) == 0 || setup[0] != bsStart {
				setup = append([]string{bsStart}, setup...)
			}
			if _, err := bsFeed(a, setup...); err != nil {
				t.Fatalf("setup: %v", err)
			}
			out, err := a.TransformStreamLine([]byte("data: " + tc.line))
			if err != errStreamTransformAborted {
				t.Fatalf("err = %v, want errStreamTransformAborted", err)
			}
			if len(out) != 0 {
				t.Errorf("abort must not emit lines, got %q", out)
			}
		})
	}

	t.Run("tool block cap", func(t *testing.T) {
		t.Parallel()
		a := &BedrockAdapter{}
		if _, err := bsFeed(a, bsStart); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < maxAnthropicToolBlocks; i++ {
			if _, err := a.TransformStreamLine([]byte("data: " + bsToolStart(i, fmt.Sprintf("t%d", i), "f"))); err != nil {
				t.Fatalf("block %d rejected: %v", i, err)
			}
		}
		if _, err := a.TransformStreamLine([]byte("data: " + bsToolStart(maxAnthropicToolBlocks, "over", "f"))); err != errStreamTransformAborted {
			t.Errorf("65th tool block err = %v, want abort", err)
		}
	})

	t.Run("line without data prefix", func(t *testing.T) {
		t.Parallel()
		for _, l := range []string{`{"messageStart":{}}`, "event: x", ": comment", "data:{}", "DATA: {}", " data: {}"} {
			if _, err := (&BedrockAdapter{}).TransformStreamLine([]byte(l)); err != errStreamTransformAborted {
				t.Errorf("%q: err = %v, want abort", l, err)
			}
		}
	})
}

func TestBedrockAdapter_StreamDroppedEvents(t *testing.T) {
	t.Parallel()
	drops := []string{
		bsBlkStop,
		`{"p":"abcdefghijklmnop"}`,
		`{"unknownEvent":{"x":1}}`,
		`{}`,
		`{"contentBlockStart":{"contentBlockIndex":0,"start":{}}}`,
		`{"contentBlockStart":{"contentBlockIndex":0,"start":{"toolResult":{"toolUseId":"a"}}}}`,
		`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"SECRET-THINKING"}}}}`,
		`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"citation":{}}}}`,
		`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":""}}}`,
		`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{}}}`,
	}
	for i, d := range drops {
		a := &BedrockAdapter{startSeen: true}
		out, err := a.TransformStreamLine([]byte("data: " + d))
		if err != nil {
			t.Errorf("drop %d: err = %v", i, err)
		}
		if len(out) != 0 {
			t.Errorf("drop %d: emitted %q", i, out)
		}
	}
}

func TestBedrockAdapter_StreamBlankLinesPassedThrough(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	for _, in := range [][]byte{nil, {}} {
		out, err := a.TransformStreamLine(in)
		if err != nil || len(out) != 1 || len(out[0]) != 0 {
			t.Errorf("blank = %q, %v", out, err)
		}
	}
}

func TestBedrockAdapter_StreamPaddingDoesNotDisturbState(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	lines, err := bsFeed(a, bsStart, `{"p":"aaaa"}`, bsText(0, "x"), `{"p":"bbbb"}`, bsStop, `{"p":"c"}`, bsMeta)
	if err != nil {
		t.Fatal(err)
	}
	if lines[len(lines)-2] != "data: [DONE]" {
		t.Errorf("tail = %q", lines[len(lines)-2:])
	}
}

func TestBedrockAdapter_StreamTextEscaping(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	text := "line1\nline2 \"q\" \\ <tag> ä  "
	lines, err := bsFeed(a, bsStart, bsText(0, text))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if strings.Contains(l, "\n") {
			t.Errorf("output line contains raw newline: %q", l)
		}
	}
	chunks := bsChunks(t, lines)
	if len(chunks) != 2 || bsDelta(chunks[1])["content"] != text {
		t.Errorf("round trip failed: %v", chunks)
	}
}

func TestBedrockAdapter_StreamUsageArithmetic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		meta string
		want UsageInfo
	}{
		{"plain", `{"metadata":{"usage":{"inputTokens":7,"outputTokens":3}}}`, UsageInfo{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}},
		{"cache read", `{"metadata":{"usage":{"inputTokens":7,"outputTokens":3,"cacheReadInputTokens":50}}}`, UsageInfo{PromptTokens: 57, CompletionTokens: 3, TotalTokens: 60, CachedReadTokens: 50}},
		{"cache write", `{"metadata":{"usage":{"inputTokens":7,"outputTokens":3,"cacheWriteInputTokens":9}}}`, UsageInfo{PromptTokens: 16, CompletionTokens: 3, TotalTokens: 19, CacheWriteTokens: 9}},
		{"both", `{"metadata":{"usage":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheWriteInputTokens":4}}}`, UsageInfo{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10, CachedReadTokens: 3, CacheWriteTokens: 4}},
		{"empty usage", `{"metadata":{}}`, UsageInfo{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &BedrockAdapter{}
			if got := a.StreamUsage(); got != (UsageInfo{}) {
				t.Errorf("usage before metadata = %+v, want zero", got)
			}
			if _, err := bsFeed(a, tc.meta); err != nil {
				t.Fatal(err)
			}
			if got := a.StreamUsage(); got != tc.want {
				t.Errorf("StreamUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBedrockAdapter_StreamIDFallback(t *testing.T) {
	t.Parallel()
	a := &BedrockAdapter{}
	lines, err := bsFeed(a, bsStart)
	if err != nil {
		t.Fatal(err)
	}
	if c := bsChunks(t, lines); len(c) != 1 || c[0]["id"] == "" {
		t.Errorf("chunk id missing: %v", c)
	}
}

func TestBedrockAdapter_StreamLifecycleStrictness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
	}{
		{"null messageStart", []string{`{"messageStart":null}`}},
		{"null contentBlockDelta", []string{bsStart, `{"contentBlockDelta":null}`}},
		{"null contentBlockStart", []string{bsStart, `{"contentBlockStart":null}`}},
		{"null contentBlockStop", []string{bsStart, `{"contentBlockStop":null}`}},
		{"null messageStop", []string{bsStart, `{"messageStop":null}`}},
		{"null metadata", []string{bsStart, `{"metadata":null}`}},
		{"null exception", []string{`{":exception":null}`}},
		{"delta before messageStart", []string{bsText(0, "x")}},
		{"start before messageStart", []string{bsToolStart(0, "a", "f")}},
		{"stop before messageStart", []string{bsBlkStop}},
		{"duplicate contentBlockStop", []string{bsStart, bsBlkStop, bsBlkStop}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := bsFeed(&BedrockAdapter{}, tc.lines...); err != errStreamTransformAborted {
				t.Errorf("err = %v, want errStreamTransformAborted", err)
			}
		})
	}
}
