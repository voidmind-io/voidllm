package proxy

import (
	"bytes"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// bedrockStreamEvent is one decoded ConverseStream event as produced by the
// event-stream reader: a JSON object with exactly one member named after the
// event type, or {":exception":"<name>"} for an in-stream exception. Members
// that are not modeled here (padding, reasoning, citations) are ignored.
type bedrockStreamEvent struct {
	MessageStart      *bedrockMessageStartEvent      `json:"messageStart"`
	ContentBlockStart *bedrockContentBlockStartEvent `json:"contentBlockStart"`
	ContentBlockDelta *bedrockContentBlockDeltaEvent `json:"contentBlockDelta"`
	ContentBlockStop  *bedrockContentBlockStopEvent  `json:"contentBlockStop"`
	MessageStop       *bedrockMessageStopEvent       `json:"messageStop"`
	Metadata          *bedrockMetadataEvent          `json:"metadata"`
	Exception         *string                        `json:":exception"`
}

// bedrockStreamMembers lists the event members the adapter recognizes.
var bedrockStreamMembers = [...]string{
	"messageStart", "contentBlockStart", "contentBlockDelta", "contentBlockStop",
	"messageStop", "metadata", ":exception",
}

// bedrockMessageStartEvent is the messageStart event payload.
type bedrockMessageStartEvent struct {
	Role string `json:"role"`
}

// bedrockContentBlockStartEvent is the contentBlockStart event payload.
type bedrockContentBlockStartEvent struct {
	Index *int `json:"contentBlockIndex"`
	Start struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId"`
			Name      string `json:"name"`
		} `json:"toolUse"`
	} `json:"start"`
}

// bedrockContentBlockStopEvent is the contentBlockStop event payload.
type bedrockContentBlockStopEvent struct {
	Index *int `json:"contentBlockIndex"`
}

// bedrockContentBlockDeltaEvent is the contentBlockDelta event payload.
type bedrockContentBlockDeltaEvent struct {
	Index *int `json:"contentBlockIndex"`
	Delta struct {
		Text    *string `json:"text"`
		ToolUse *struct {
			Input string `json:"input"`
		} `json:"toolUse"`
	} `json:"delta"`
}

// bedrockMessageStopEvent is the messageStop event payload.
type bedrockMessageStopEvent struct {
	StopReason string `json:"stopReason"`
}

// bedrockMetadataEvent is the metadata event payload carrying token usage.
type bedrockMetadataEvent struct {
	Usage bedrockUsage `json:"usage"`
}

// TransformStreamLine converts one SSE line produced by the event-stream
// reader into zero or more OpenAI SSE lines. Blank lines pass through. Any
// protocol violation or in-stream exception aborts the stream fail-closed with
// errStreamTransformAborted, which carries no content.
//
// Bedrock delivers messageStop and metadata as two events whose order is not
// guaranteed; the finish chunk is emitted at messageStop and the optional
// usage chunk plus [DONE] once both have arrived, so usage is never lost.
func (a *BedrockAdapter) TransformStreamLine(line []byte) ([][]byte, error) {
	if len(line) == 0 {
		return [][]byte{line}, nil
	}
	payload, ok := bytes.CutPrefix(line, []byte("data: "))
	if !ok {
		return nil, errStreamTransformAborted
	}

	var ev bedrockStreamEvent
	if err := jsonx.Unmarshal(payload, &ev); err != nil {
		return nil, errStreamTransformAborted
	}

	// A recognized member whose value is JSON null decodes to a nil pointer
	// and would look absent; treat it as a protocol violation instead.
	var members map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(payload, &members); err != nil {
		return nil, errStreamTransformAborted
	}
	for _, key := range bedrockStreamMembers {
		if raw, present := members[key]; present && isJSONNull(raw) {
			return nil, errStreamTransformAborted
		}
	}

	switch {
	case ev.Exception != nil:
		return nil, errStreamTransformAborted

	case ev.MessageStart != nil:
		if a.startSeen || a.stopSeen {
			return nil, errStreamTransformAborted
		}
		a.startSeen = true
		chunk := a.buildChunk(openAIChunkDelta{Role: "assistant"}, nil)
		return singleLine(chunk), nil

	case ev.ContentBlockStart != nil:
		return a.handleContentBlockStart(ev.ContentBlockStart)

	case ev.ContentBlockDelta != nil:
		return a.handleContentBlockDelta(ev.ContentBlockDelta)

	case ev.ContentBlockStop != nil:
		idx := ev.ContentBlockStop.Index
		if idx == nil || *idx < 0 || !a.startSeen || a.stopSeen || a.blockStopped(*idx) {
			return nil, errStreamTransformAborted
		}
		if a.stoppedBlocks == nil {
			a.stoppedBlocks = make(map[int]struct{})
		}
		a.stoppedBlocks[*idx] = struct{}{}
		return nil, nil

	case ev.MessageStop != nil:
		if a.stopSeen {
			return nil, errStreamTransformAborted
		}
		a.stopSeen = true
		reason := mapBedrockStopReason(ev.MessageStop.StopReason)
		lines := singleLine(a.buildChunk(openAIChunkDelta{}, &reason))
		if a.metadataSeen {
			lines = append(lines, a.finishLines()...)
		}
		return lines, nil

	case ev.Metadata != nil:
		if a.metadataSeen {
			return nil, errStreamTransformAborted
		}
		if !ev.Metadata.Usage.valid() {
			return nil, errStreamTransformAborted
		}
		a.metadataSeen = true
		a.inputTokens = ev.Metadata.Usage.InputTokens
		a.outputTokens = ev.Metadata.Usage.OutputTokens
		a.cacheReadTokens = ev.Metadata.Usage.CacheReadInputTokens
		a.cacheWriteTokens = ev.Metadata.Usage.CacheWriteInputTokens
		if a.stopSeen {
			return a.finishLines(), nil
		}
		return nil, nil

	default:
		// Padding-only and unknown events carry nothing to forward.
		return nil, nil
	}
}

// handleContentBlockStart registers a tool-use block and emits the OpenAI
// tool_calls header delta. Non-tool block starts are dropped.
func (a *BedrockAdapter) handleContentBlockStart(ev *bedrockContentBlockStartEvent) ([][]byte, error) {
	if ev.Index == nil || *ev.Index < 0 || !a.startSeen || a.stopSeen || a.blockStopped(*ev.Index) {
		return nil, errStreamTransformAborted
	}
	index := *ev.Index
	tu := ev.Start.ToolUse
	if tu == nil {
		return nil, nil
	}
	if a.toolCallCounter >= maxAnthropicToolBlocks {
		return nil, errStreamTransformAborted
	}
	if _, exists := a.blockToToolCall[index]; exists {
		return nil, errStreamTransformAborted
	}
	if !bedrockToolUseIDRe.MatchString(tu.ToolUseID) || !bedrockToolNameRe.MatchString(tu.Name) {
		return nil, errStreamTransformAborted
	}
	if isForwardedPseudonym(tu.ToolUseID) || isForwardedPseudonym(tu.Name) {
		return nil, errStreamTransformAborted
	}

	if a.blockToToolCall == nil {
		a.blockToToolCall = make(map[int]int)
	}
	idx := a.toolCallCounter
	a.blockToToolCall[index] = idx
	a.toolCallCounter++

	tc := openAIToolCall{
		Index: &idx,
		ID:    tu.ToolUseID,
		Type:  "function",
		Function: openAIToolCallFunction{
			Name:      tu.Name,
			Arguments: "",
		},
	}
	return singleLine(a.buildChunk(openAIChunkDelta{ToolCalls: []openAIToolCall{tc}}, nil)), nil
}

// handleContentBlockDelta emits a content chunk for text deltas and an
// argument fragment for tool input deltas. Other delta kinds are dropped.
func (a *BedrockAdapter) handleContentBlockDelta(ev *bedrockContentBlockDeltaEvent) ([][]byte, error) {
	if ev.Index == nil || *ev.Index < 0 || !a.startSeen || a.stopSeen || a.blockStopped(*ev.Index) {
		return nil, errStreamTransformAborted
	}
	switch {
	case ev.Delta.Text != nil:
		if *ev.Delta.Text == "" {
			return nil, nil
		}
		return singleLine(a.buildChunk(openAIChunkDelta{Content: *ev.Delta.Text}, nil)), nil

	case ev.Delta.ToolUse != nil:
		idx, ok := a.blockToToolCall[*ev.Index]
		if !ok {
			return nil, errStreamTransformAborted
		}
		tc := openAIToolCall{
			Index:    &idx,
			Function: openAIToolCallFunction{Arguments: ev.Delta.ToolUse.Input},
		}
		return singleLine(a.buildChunk(openAIChunkDelta{ToolCalls: []openAIToolCall{tc}}, nil)), nil

	default:
		return nil, nil
	}
}

// blockStopped reports whether a contentBlockStop was seen for index.
func (a *BedrockAdapter) blockStopped(index int) bool {
	_, ok := a.stoppedBlocks[index]
	return ok
}

// finishLines returns the closing lines of a stream: the optional usage-only
// chunk requested via stream_options.include_usage, then [DONE].
func (a *BedrockAdapter) finishLines() [][]byte {
	var lines [][]byte
	if a.includeUsage {
		if usage := a.buildUsageChunk(); usage != nil {
			lines = append(lines, appendDataPrefix(usage))
		}
	}
	return append(lines, []byte("data: [DONE]"))
}

// StreamUsage returns the token counts reported by the metadata event. The
// cache buckets are added into PromptTokens, matching the buffered path.
func (a *BedrockAdapter) StreamUsage() UsageInfo {
	prompt := a.inputTokens + a.cacheReadTokens + a.cacheWriteTokens
	return UsageInfo{
		PromptTokens:     prompt,
		CompletionTokens: a.outputTokens,
		TotalTokens:      prompt + a.outputTokens,
		CachedReadTokens: a.cacheReadTokens,
		CacheWriteTokens: a.cacheWriteTokens,
	}
}

// streamID returns the id used for every chunk of this stream.
func (a *BedrockAdapter) streamID() string {
	if a.responseID == "" {
		return "chatcmpl-proxy"
	}
	return a.responseID
}

// buildChunk assembles an OpenAI streaming chunk. It returns nil on a marshal
// failure.
func (a *BedrockAdapter) buildChunk(delta openAIChunkDelta, finishReason *string) []byte {
	out, err := jsonx.Marshal(openAIChunk{
		ID:     a.streamID(),
		Object: "chat.completion.chunk",
		Choices: []openAIChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	})
	if err != nil {
		return nil
	}
	return out
}

// buildUsageChunk assembles the trailing usage-only chunk with an empty
// choices array, matching OpenAI's include_usage shape. It returns nil on a
// marshal failure.
func (a *BedrockAdapter) buildUsageChunk() []byte {
	ui := a.StreamUsage()
	created := time.Now().Unix()
	model := a.modelName
	if model == "" {
		model = "bedrock"
	}
	out, err := jsonx.Marshal(openAIChunk{
		ID:      a.streamID(),
		Object:  "chat.completion.chunk",
		Created: &created,
		Model:   &model,
		Choices: []openAIChunkChoice{},
		Usage: &openAIUsage{
			PromptTokens:        ui.PromptTokens,
			CompletionTokens:    ui.CompletionTokens,
			TotalTokens:         ui.TotalTokens,
			PromptTokensDetails: cacheUsageDetails(ui.CachedReadTokens, ui.CacheWriteTokens),
		},
	})
	if err != nil {
		return nil
	}
	return out
}

// singleLine wraps chunk as one "data: " SSE line, or returns nil when the
// chunk failed to marshal.
func singleLine(chunk []byte) [][]byte {
	if chunk == nil {
		return nil
	}
	return [][]byte{appendDataPrefix(chunk)}
}
