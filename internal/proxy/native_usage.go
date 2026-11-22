package proxy

import (
	"bytes"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// messagesUsage is the usage object of the Anthropic Messages API. Cache read
// and cache creation tokens are reported IN ADDITION TO input_tokens (see
// UsageInfo's doc).
type messagesUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// info converts the Anthropic usage into the normalized UsageInfo, using the
// same additive convention as AnthropicAdapter: PromptTokens is the sum of
// input, cache-read and cache-creation tokens.
func (u messagesUsage) info() UsageInfo {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return UsageInfo{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
		CachedReadTokens: u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// responsesUsage is the usage object of the OpenAI Responses API. cached_tokens
// is a subset of input_tokens, and output_tokens already includes reasoning
// tokens.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// info converts the Responses usage into the normalized UsageInfo.
func (u responsesUsage) info() UsageInfo {
	ui := UsageInfo{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if ui.TotalTokens == 0 {
		ui.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.InputTokensDetails != nil {
		ui.CachedReadTokens = u.InputTokensDetails.CachedTokens
	}
	return ui
}

// parseMessagesUsage extracts the usage of a non-streaming Anthropic Messages
// response body. It returns a zero UsageInfo when the body is not JSON or has
// no usage object.
func parseMessagesUsage(body []byte) UsageInfo {
	var resp struct {
		Usage *messagesUsage `json:"usage"`
	}
	if jsonx.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return UsageInfo{}
	}
	return resp.Usage.info()
}

// parseResponsesUsage extracts the usage of a non-streaming OpenAI Responses
// response body. It returns a zero UsageInfo when the body is not JSON or has
// no usage object.
func parseResponsesUsage(body []byte) UsageInfo {
	var resp struct {
		Usage *responsesUsage `json:"usage"`
	}
	if jsonx.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return UsageInfo{}
	}
	return resp.Usage.info()
}

// usageFromNativeBody extracts the usage of a non-streaming response of the
// native endpoint f. It returns a zero UsageInfo for other formats.
func usageFromNativeBody(f apiFormat, body []byte) UsageInfo {
	switch f {
	case formatMessages:
		return parseMessagesUsage(body)
	case formatResponses:
		return parseResponsesUsage(body)
	}
	return UsageInfo{}
}

// nativeTerminal classifies the terminal event of a native stream.
type nativeTerminal int

const (
	// termNone means the event is not terminal.
	termNone nativeTerminal = iota
	// termOK is a clean end of stream: message_stop or response.completed.
	termOK
	// termIncomplete is response.incomplete: the response ended early (for
	// example by hitting the output token limit). The upstream is healthy.
	termIncomplete
	// termFailed is an upstream-reported failure: an error event or
	// response.failed.
	termFailed
)

// nativeStream observes the SSE data lines of a native Messages or Responses
// stream. It extracts usage and recognizes the terminal event. It never keeps
// or logs any content. A nativeStream must not be shared between requests.
type nativeStream struct {
	format apiFormat

	// Messages: cumulative usage. message_start carries input and cache
	// tokens, message_delta the cumulative output tokens.
	msg messagesUsage
	// Responses: usage of the terminal response object.
	resp UsageInfo

	// pending is the terminal classification of the SSE event currently being
	// read; endEvent returns and clears it.
	pending nativeTerminal
}

// observe processes one raw SSE line. Lines that are not JSON data lines are
// ignored.
func (n *nativeStream) observe(line []byte) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimLeft(data, " ")
	if len(data) == 0 || data[0] != '{' {
		return
	}

	typ := nativeEventType(data)
	switch n.format {
	case formatMessages:
		switch string(typ) {
		case "message_start":
			var ev struct {
				Message struct {
					Usage messagesUsage `json:"usage"`
				} `json:"message"`
			}
			if jsonx.Unmarshal(data, &ev) == nil {
				n.msg.InputTokens = ev.Message.Usage.InputTokens
				n.msg.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
				n.msg.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
				if ev.Message.Usage.OutputTokens > n.msg.OutputTokens {
					n.msg.OutputTokens = ev.Message.Usage.OutputTokens
				}
			}
		case "message_delta":
			// Newer API versions report cumulative input and cache figures here
			// as well; the larger of start and delta wins, output comes from the
			// delta.
			var ev struct {
				Usage messagesUsage `json:"usage"`
			}
			if jsonx.Unmarshal(data, &ev) == nil {
				n.msg.InputTokens = max(n.msg.InputTokens, ev.Usage.InputTokens)
				n.msg.CacheReadInputTokens = max(n.msg.CacheReadInputTokens, ev.Usage.CacheReadInputTokens)
				n.msg.CacheCreationInputTokens = max(n.msg.CacheCreationInputTokens, ev.Usage.CacheCreationInputTokens)
				if ev.Usage.OutputTokens > 0 {
					n.msg.OutputTokens = ev.Usage.OutputTokens
				}
			}
		case "message_stop":
			n.pending = termOK
		case "error":
			n.pending = termFailed
		}
	case formatResponses:
		switch string(typ) {
		case "response.completed", "response.incomplete", "response.failed":
			var ev struct {
				Response struct {
					Usage *responsesUsage `json:"usage"`
				} `json:"response"`
			}
			if jsonx.Unmarshal(data, &ev) == nil && ev.Response.Usage != nil {
				n.resp = ev.Response.Usage.info()
			}
			switch string(typ) {
			case "response.completed":
				n.pending = termOK
			case "response.incomplete":
				n.pending = termIncomplete
			default:
				n.pending = termFailed
			}
		case "error":
			n.pending = termFailed
		}
	}
}

// endEvent is called at an SSE event boundary. It returns the terminal
// classification of the event that just ended and resets it.
func (n *nativeStream) endEvent() nativeTerminal {
	t := n.pending
	n.pending = termNone
	return t
}

// usage returns the usage accumulated so far.
func (n *nativeStream) usage() UsageInfo {
	if n.format == formatMessages {
		return n.msg.info()
	}
	return n.resp
}

// nativeEventType returns the "type" field of a JSON SSE payload. Upstreams emit
// it first, so the common case is answered without parsing the document; other
// layouts fall back to a full parse.
func nativeEventType(data []byte) []byte {
	const prefix = `{"type":"`
	if rest, ok := bytes.CutPrefix(data, []byte(prefix)); ok {
		if end := bytes.IndexByte(rest, '"'); end >= 0 {
			return rest[:end]
		}
	}
	var ev struct {
		Type string `json:"type"`
	}
	if jsonx.Unmarshal(data, &ev) != nil {
		return nil
	}
	return []byte(ev.Type)
}
