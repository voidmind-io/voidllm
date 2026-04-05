package proxy

import (
	"bytes"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// bedrockEventStreamReader wraps a raw Amazon Bedrock Event Stream response
// body and exposes it as an io.ReadCloser that produces OpenAI-compatible SSE
// lines. It is consumed by the proxy handler's SSE scanner, which reads lines
// separated by newline characters.
//
// AWS Event Stream frames are decoded one at a time. For each frame the
// :event-type header determines what OpenAI SSE lines to emit:
//
//   - messageStart        → emit role chunk
//   - contentBlockDelta   → emit content chunk
//   - messageStop         → emit finish chunk + "data: [DONE]"
//   - metadata            → extract usage counts, no SSE output
//   - all others          → skip silently
type bedrockEventStreamReader struct {
	src     io.ReadCloser
	decoder *eventstream.Decoder
	adapter *BedrockConverseAdapter // for accumulating token counts
	buf     bytes.Buffer            // buffered SSE output not yet consumed by Read
	done    bool                    // true once [DONE] has been emitted
}

// newBedrockEventStreamReader creates a reader that converts the binary event
// stream in src into OpenAI SSE lines. adapter receives the accumulated token
// counts extracted from metadata events.
func newBedrockEventStreamReader(src io.ReadCloser, adapter *BedrockConverseAdapter) *bedrockEventStreamReader {
	return &bedrockEventStreamReader{
		src:     src,
		decoder: eventstream.NewDecoder(),
		adapter: adapter,
	}
}

// Read implements io.Reader. It decodes Event Stream frames from the upstream
// body, converts them to SSE lines, and copies the result into p. When the
// event stream ends cleanly, Read returns (n, io.EOF).
func (r *bedrockEventStreamReader) Read(p []byte) (int, error) {
	for r.buf.Len() == 0 {
		if r.done {
			return 0, io.EOF
		}

		msg, err := r.decoder.Decode(r.src, nil)
		if err != nil {
			if err == io.EOF {
				r.done = true
				return 0, io.EOF
			}
			return 0, err
		}

		r.processMessage(msg)
	}

	return r.buf.Read(p)
}

// Close closes the underlying upstream response body.
func (r *bedrockEventStreamReader) Close() error {
	return r.src.Close()
}

// processMessage converts a single Event Stream message into zero or more SSE
// lines and appends them to r.buf. Each SSE line is terminated with "\n"; the
// handler's scanner uses "\n" as the delimiter.
func (r *bedrockEventStreamReader) processMessage(msg eventstream.Message) {
	eventType := headerStringValue(msg.Headers, ":event-type")

	switch eventType {
	case "messageStart":
		// Emit a role chunk with delta.role = "assistant".
		chunk := openAIChunk{
			ID:     "chatcmpl-bedrock",
			Object: "chat.completion.chunk",
			Choices: []openAIChunkChoice{
				{
					Index: 0,
					Delta: openAIChunkDelta{Role: "assistant"},
				},
			},
		}
		r.emitChunk(chunk)

	case "contentBlockDelta":
		// Extract delta.text from the payload.
		var payload struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := jsonx.Unmarshal(msg.Payload, &payload); err != nil {
			return
		}
		if payload.Delta.Type != "text" && payload.Delta.Type != "text_delta" {
			return
		}
		chunk := openAIChunk{
			ID:     "chatcmpl-bedrock",
			Object: "chat.completion.chunk",
			Choices: []openAIChunkChoice{
				{
					Index: 0,
					Delta: openAIChunkDelta{Content: payload.Delta.Text},
				},
			},
		}
		r.emitChunk(chunk)

	case "messageStop":
		// Emit a finish chunk then [DONE].
		var payload struct {
			StopReason string `json:"stopReason"`
		}
		_ = jsonx.Unmarshal(msg.Payload, &payload)
		reason := mapBedrockStopReason(payload.StopReason)
		chunk := openAIChunk{
			ID:     "chatcmpl-bedrock",
			Object: "chat.completion.chunk",
			Choices: []openAIChunkChoice{
				{
					Index:        0,
					Delta:        openAIChunkDelta{},
					FinishReason: &reason,
				},
			},
		}
		r.emitChunk(chunk)
		r.buf.WriteString("data: [DONE]\n\n")
		r.done = true

	case "metadata":
		// Extract usage counts but emit no SSE output.
		var payload struct {
			Usage struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"usage"`
		}
		if err := jsonx.Unmarshal(msg.Payload, &payload); err != nil {
			return
		}
		if r.adapter != nil {
			r.adapter.inputTokens = payload.Usage.InputTokens
			r.adapter.outputTokens = payload.Usage.OutputTokens
		}

	default:
		// contentBlockStart, contentBlockStop, and any unknown types are skipped.
	}
}

// emitChunk marshals chunk into a "data: {...}\n\n" SSE line and appends it to
// r.buf. Marshalling errors are silently dropped — a single malformed chunk is
// not fatal to the stream.
func (r *bedrockEventStreamReader) emitChunk(chunk openAIChunk) {
	b, err := jsonx.Marshal(chunk)
	if err != nil {
		return
	}
	r.buf.WriteString("data: ")
	r.buf.Write(b)
	r.buf.WriteString("\n\n")
}

// headerStringValue returns the string value of the Event Stream message header
// with the given name, or an empty string if the header is absent.
func headerStringValue(headers eventstream.Headers, name string) string {
	for _, h := range headers {
		if h.Name == name {
			if sv, ok := h.Value.(eventstream.StringValue); ok {
				return string(sv)
			}
		}
	}
	return ""
}
