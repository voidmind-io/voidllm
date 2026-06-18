package pii

import (
	"bytes"
	"errors"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// dataLinePrefix is the SSE field prefix per RFC 6202 §4.2. A "data" field
// line MUST begin with "data:" — the space after the colon is optional.
var dataLinePrefix = []byte("data:")

// errUnparseableSSE is returned by RestoreSSEStream when a data: line carries
// JSON that cannot be parsed for content-aware restore. The caller must treat
// this as fail-closed and must not forward un-restored content to the client.
var errUnparseableSSE = errors.New("pii: sse stream structure could not be parsed for content-aware restore")

// choiceBuf accumulates per-choice streaming content across SSE events.
type choiceBuf struct {
	content      bytes.Buffer
	finishReason string // empty = none seen
	role         string // preserved from delta.role, first occurrence only
}

// RestoreSSEStream performs content-aware PII restore over a buffered set of
// raw SSE lines. It is the correct fix for the Stage-0a raw-byte-join bug:
// a pseudonym split across two SSE events by the LLM tokenizer never appears
// as a contiguous byte sequence in the raw-joined buffer, so strings.Replacer
// cannot find and replace it. This function instead extracts the content string
// from each event's JSON payload, concatenates per choice index, applies
// restore over the assembled string (where the pseudonym is always contiguous),
// and re-emits a valid SSE response.
//
// Input contract: sseLines is the complete set of raw lines read from the
// upstream SSE body (one element per scanner.Scan() call, without trailing \n).
// The restore function is Filter.Restore — it is applied to the assembled
// content bytes for each choice and returns the restored bytes.
//
// Output: a new slice of raw SSE lines (without trailing \n) ready to be
// written to the client. nil elements in the output represent blank separator
// lines between events.
//
// Fail-closed contract:
//   - Any data: line whose payload is not [DONE] and not valid JSON → error.
//   - Any choice that carries tool_calls deltas → error (cannot safely
//     reassemble split tool-call argument JSON strings across events).
//   - On any error, the caller must not emit any content to the client.
//
// Belt-and-suspenders restore: after synthesizing all output lines, restore is
// applied a final time to every emitted byte. This ensures that pseudonyms
// echo-ed by the upstream in envelope fields (id, model, system_fingerprint),
// SSE comment lines, or any other non-content field are removed even if they
// were not captured during the content-assembly phase. restore is idempotent:
// it only substitutes known pseudonyms and never rewrites already-restored
// content.
//
// Stage 0b note: the restored content is emitted as a single delta chunk per
// choice (no incremental token streaming for PII-hit requests). This is
// correct and leak-free. Stage 0b would implement a cross-event rolling buffer
// that delivers restored content incrementally; that is a performance
// optimisation, not a correctness fix.
//
// n>1 choices: each choice index is handled independently. The output emits
// all choices in index order.
func RestoreSSEStream(sseLines [][]byte, restore func([]byte) []byte) ([][]byte, error) {
	donePayload := []byte("[DONE]")

	// nonDataLines collects lines that are not data: JSON events (blank lines,
	// SSE comment lines, event:/id:/retry: lines). They are emitted verbatim
	// at the start of the output, before synthesized content chunks.
	var nonDataLines [][]byte

	// envelope holds the top-level SSE chunk fields (id, object, created,
	// model, system_fingerprint) captured from the first parseable data chunk.
	var envelope map[string]jsonx.RawMessage

	// choicesByIndex accumulates content and metadata per OpenAI choice index.
	choicesByIndex := make(map[int]*choiceBuf)

	var hasToolCalls bool

	for _, line := range sseLines {
		// Per SSE spec (RFC 6202 §4.2): a "data" field line begins with "data:"
		// followed by an optional single space. We must check for the bare
		// "data:" prefix (no space) to avoid silently emitting content-carrying
		// lines that don't have the conventional space.
		if !bytes.HasPrefix(line, dataLinePrefix) {
			// Not a data: line. Lines starting with "event:", "id:", "retry:",
			// or ":" (comment) carry no content and are safe to pass through.
			nonDataLines = append(nonDataLines, line)
			continue
		}

		// Strip "data:" and then an optional single leading space per SSE spec.
		payload := line[len(dataLinePrefix):]
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}

		// [DONE] sentinel: not JSON, skip; re-emitted at end of output.
		if bytes.Equal(payload, donePayload) {
			continue
		}

		// Any data: line whose payload is not [DONE] and not valid JSON is a
		// protocol violation that we cannot safely process. Fail-closed: return
		// an error rather than emitting un-restored content.
		// (The JSON parse below covers this; this comment documents intent.)

		// Parse the raw document to capture envelope fields.
		var rawDoc map[string]jsonx.RawMessage
		if err := jsonx.Unmarshal(payload, &rawDoc); err != nil {
			return nil, errUnparseableSSE
		}

		// Capture envelope fields from the first data chunk that has them.
		if envelope == nil {
			envelope = make(map[string]jsonx.RawMessage)
			for _, field := range []string{"id", "object", "created", "model", "system_fingerprint"} {
				if v, ok := rawDoc[field]; ok {
					envelope[field] = v
				}
			}
		}

		// Chunks without "choices" carry auxiliary data (e.g. usage with
		// stream_options.include_usage=true). Skip content accumulation.
		rawChoicesJSON, hasChoices := rawDoc["choices"]
		if !hasChoices {
			continue
		}

		// Parse choices as a typed slice for safe field extraction.
		var rawChoices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Role      string             `json:"role"`
				Content   *string            `json:"content"`
				ToolCalls []jsonx.RawMessage `json:"tool_calls,omitempty"`
			} `json:"delta"`
			// Completions streaming uses "text" at the choice level instead of delta.
			Text *string `json:"text"`
		}
		if err := jsonx.Unmarshal(rawChoicesJSON, &rawChoices); err != nil {
			return nil, errUnparseableSSE
		}

		for _, ch := range rawChoices {
			// Fail-closed: tool_calls deltas span multiple chunks and their
			// function.arguments JSON string may be split. Reassembling them
			// safely is not implemented in Stage 0a/0b.
			if len(ch.Delta.ToolCalls) > 0 {
				hasToolCalls = true
			}

			cb, exists := choicesByIndex[ch.Index]
			if !exists {
				cb = &choiceBuf{}
				choicesByIndex[ch.Index] = cb
			}

			if ch.Delta.Role != "" && cb.role == "" {
				cb.role = ch.Delta.Role
			}
			if ch.Delta.Content != nil {
				cb.content.WriteString(*ch.Delta.Content)
			}
			if ch.Text != nil {
				cb.content.WriteString(*ch.Text)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				cb.finishReason = *ch.FinishReason
			}
		}
	}

	if hasToolCalls {
		// Fail-closed: cannot safely restore split tool-call argument content.
		return nil, errUnparseableSSE
	}

	// Build output SSE lines.
	// Capacity estimate: non-data lines + 2 lines per choice (content + finish)
	// + 2 for [DONE] + separator blanks.
	out := make([][]byte, 0, len(nonDataLines)+len(choicesByIndex)*4+2)

	// Preserve non-data lines at the top, applying restore to each so that
	// pseudonyms echo-ed in SSE comment or event: lines are removed.
	for _, ndl := range nonDataLines {
		if len(ndl) > 0 {
			out = append(out, restore(ndl))
		} else {
			out = append(out, ndl)
		}
	}

	if envelope == nil {
		// Stream had no parseable data chunks at all; emit a minimal envelope.
		envelope = map[string]jsonx.RawMessage{
			"object": jsonx.RawMessage(`"chat.completion.chunk"`),
		}
	}

	// Sort choice indices for deterministic output order.
	indices := sortedKeys(choicesByIndex)

	for _, idx := range indices {
		cb := choicesByIndex[idx]

		// Apply restore to the assembled content for this choice.
		restoredBytes := restore(cb.content.Bytes())

		// Emit content delta chunk.
		contentLine, err := buildContentChunk(envelope, idx, cb.role, string(restoredBytes))
		if err != nil {
			return nil, errUnparseableSSE
		}
		out = append(out, contentLine)
		out = append(out, nil) // blank SSE event separator

		// Emit finish_reason chunk when one was seen during accumulation.
		if cb.finishReason != "" {
			frLine, err := buildFinishChunk(envelope, idx, cb.finishReason)
			if err != nil {
				return nil, errUnparseableSSE
			}
			out = append(out, frLine)
			out = append(out, nil) // blank SSE event separator
		}
	}

	// Always close with [DONE].
	out = append(out, []byte("data: [DONE]"))
	out = append(out, nil) // blank SSE event separator

	// Belt-and-suspenders final pass: apply restore to every non-nil output
	// line. This catches any pseudonym that may have been embedded in envelope
	// fields (id, model, system_fingerprint) during JSON serialization of the
	// synthesized chunks. restore is idempotent — already-restored content is
	// never modified because restored original values never match the
	// PII_<TY>_<hex> pseudonym format.
	for i, line := range out {
		if line != nil {
			out[i] = restore(line)
		}
	}

	return out, nil
}

// sseDelta is the typed representation of an SSE delta object within a choice.
// Role is omitted when empty to match the provider wire format on non-first chunks.
type sseDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

// sseDeltaEmpty is the typed representation of an empty delta used in
// finish_reason chunks (empty content, no role).
type sseDeltaEmpty struct{}

// sseContentChoice is a typed SSE choice carrying a delta.content payload.
// FinishReason is always null for content chunks.
type sseContentChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

// sseFinishChoice is a typed SSE choice carrying a finish_reason payload.
// Delta is empty (no role, no content) per the OpenAI SSE wire format.
type sseFinishChoice struct {
	Index        int           `json:"index"`
	Delta        sseDeltaEmpty `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

// buildContentChunk serializes a data: line carrying a delta.content SSE
// chunk. Envelope fields (id, object, created, model, system_fingerprint) are
// embedded verbatim from the captured first chunk so the synthesized response
// is structurally identical to a real provider response.
func buildContentChunk(envelope map[string]jsonx.RawMessage, choiceIdx int, role, content string) ([]byte, error) {
	doc := make(map[string]jsonx.RawMessage, len(envelope)+1)
	for k, v := range envelope {
		doc[k] = v
	}

	choice := sseContentChoice{
		Index:        choiceIdx,
		Delta:        sseDelta{Role: role, Content: content},
		FinishReason: nil,
	}

	choicesJSON, err := jsonx.Marshal([]sseContentChoice{choice})
	if err != nil {
		return nil, err
	}
	doc["choices"] = jsonx.RawMessage(choicesJSON)

	payload, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), payload...), nil
}

// buildFinishChunk serializes a data: line carrying a finish_reason SSE chunk
// (empty delta, non-null finish_reason) for the given choice index.
func buildFinishChunk(envelope map[string]jsonx.RawMessage, choiceIdx int, finishReason string) ([]byte, error) {
	doc := make(map[string]jsonx.RawMessage, len(envelope)+1)
	for k, v := range envelope {
		doc[k] = v
	}

	choice := sseFinishChoice{
		Index:        choiceIdx,
		Delta:        sseDeltaEmpty{},
		FinishReason: finishReason,
	}

	choicesJSON, err := jsonx.Marshal([]sseFinishChoice{choice})
	if err != nil {
		return nil, err
	}
	doc["choices"] = jsonx.RawMessage(choicesJSON)

	payload, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), payload...), nil
}

// sortedKeys returns the keys of m sorted in ascending order.
// Used to produce deterministic choice ordering in the output SSE stream.
// Insertion sort is appropriate here: choice counts are always small (1-8).
func sortedKeys(m map[int]*choiceBuf) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		key := keys[i]
		j := i - 1
		for j >= 0 && keys[j] > key {
			keys[j+1] = keys[j]
			j--
		}
		keys[j+1] = key
	}
	return keys
}
