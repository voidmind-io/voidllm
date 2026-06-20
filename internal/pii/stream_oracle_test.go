package pii

// stream_oracle_test.go contains the buffered RestoreSSEStream function
// preserved as a test oracle for property-testing the incremental StreamRestorer.
// It is NOT used in production code; it lives here so that tests can compare
// buffered vs. incremental results for the same input and assert they produce
// equivalent restored content.
//
// The oracle function and its helpers (buildContentChunkOracle, buildFinishChunkOracle,
// sortedKeys, choiceBuf, sseOracle* types) are defined only in this test file.

import (
	"bytes"
	"errors"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

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
// raw SSE lines. It is retained as a test oracle for verifying the incremental
// StreamRestorer: for any well-formed input, the restored content emitted by
// StreamRestorer must equal the restored content produced by RestoreSSEStream.
//
// Input contract: sseLines is the complete set of raw lines read from the
// upstream SSE body (one element per scanner.Scan() call, without trailing \n).
// The restore function is Filter.Restore.
//
// Output: a new slice of raw SSE lines (without trailing \n) ready to be
// written to the client. nil elements in the output represent blank separator
// lines between events.
func RestoreSSEStream(sseLines [][]byte, restore func([]byte) []byte) ([][]byte, error) {
	donePayload := []byte("[DONE]")

	var nonDataLines [][]byte
	var envelope map[string]jsonx.RawMessage
	choicesByIndex := make(map[int]*choiceBuf)
	var hasToolCalls bool

	for _, line := range sseLines {
		if !bytes.HasPrefix(line, dataLinePrefix) {
			nonDataLines = append(nonDataLines, line)
			continue
		}

		payload := line[len(dataLinePrefix):]
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}

		if bytes.Equal(payload, donePayload) {
			continue
		}

		var rawDoc map[string]jsonx.RawMessage
		if err := jsonx.Unmarshal(payload, &rawDoc); err != nil {
			return nil, errUnparseableSSE
		}

		if envelope == nil {
			envelope = make(map[string]jsonx.RawMessage)
			for _, field := range []string{"id", "object", "created", "model", "system_fingerprint"} {
				if v, ok := rawDoc[field]; ok {
					envelope[field] = v
				}
			}
		}

		rawChoicesJSON, hasChoices := rawDoc["choices"]
		if !hasChoices {
			continue
		}

		var rawChoices []struct {
			Index        int     `json:"index"`
			FinishReason *string `json:"finish_reason"`
			Delta        struct {
				Role      string             `json:"role"`
				Content   *string            `json:"content"`
				ToolCalls []jsonx.RawMessage `json:"tool_calls,omitempty"`
			} `json:"delta"`
			Text *string `json:"text"`
		}
		if err := jsonx.Unmarshal(rawChoicesJSON, &rawChoices); err != nil {
			return nil, errUnparseableSSE
		}

		for _, ch := range rawChoices {
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
		return nil, errUnparseableSSE
	}

	out := make([][]byte, 0, len(nonDataLines)+len(choicesByIndex)*4+2)

	for _, ndl := range nonDataLines {
		if len(ndl) > 0 {
			out = append(out, restore(ndl))
		} else {
			out = append(out, ndl)
		}
	}

	if envelope == nil {
		envelope = map[string]jsonx.RawMessage{
			"object": jsonx.RawMessage(`"chat.completion.chunk"`),
		}
	}

	indices := sortedKeys(choicesByIndex)

	for _, idx := range indices {
		cb := choicesByIndex[idx]
		restoredBytes := restore(cb.content.Bytes())

		contentLine, err := buildContentChunkOracle(envelope, idx, cb.role, string(restoredBytes))
		if err != nil {
			return nil, errUnparseableSSE
		}
		out = append(out, contentLine)
		out = append(out, nil)

		if cb.finishReason != "" {
			frLine, err := buildFinishChunkOracle(envelope, idx, cb.finishReason)
			if err != nil {
				return nil, errUnparseableSSE
			}
			out = append(out, frLine)
			out = append(out, nil)
		}
	}

	out = append(out, []byte("data: [DONE]"))
	out = append(out, nil)

	for i, line := range out {
		if line != nil {
			out[i] = restore(line)
		}
	}

	return out, nil
}

// sseOracleDelta is the typed delta used by the oracle builder.
type sseOracleDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

// sseOracleDeltaEmpty is an empty delta for finish chunks.
type sseOracleDeltaEmpty struct{}

// sseOracleContentChoice is a typed SSE choice for content oracle chunks.
type sseOracleContentChoice struct {
	Index        int            `json:"index"`
	Delta        sseOracleDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// sseOracleFinishChoice is a typed SSE choice for finish oracle chunks.
type sseOracleFinishChoice struct {
	Index        int                 `json:"index"`
	Delta        sseOracleDeltaEmpty `json:"delta"`
	FinishReason string              `json:"finish_reason"`
}

// buildContentChunkOracle is the oracle version of buildContentChunk: it uses
// the upstream-captured envelope (id, model, etc.) directly, which is
// intentionally not done in production code (pseudonym-echo risk).
func buildContentChunkOracle(envelope map[string]jsonx.RawMessage, choiceIdx int, role, content string) ([]byte, error) {
	doc := make(map[string]jsonx.RawMessage, len(envelope)+1)
	for k, v := range envelope {
		doc[k] = v
	}

	choice := sseOracleContentChoice{
		Index:        choiceIdx,
		Delta:        sseOracleDelta{Role: role, Content: content},
		FinishReason: nil,
	}

	choicesJSON, err := jsonx.Marshal([]sseOracleContentChoice{choice})
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

// buildFinishChunkOracle is the oracle version of buildFinishChunk.
func buildFinishChunkOracle(envelope map[string]jsonx.RawMessage, choiceIdx int, finishReason string) ([]byte, error) {
	doc := make(map[string]jsonx.RawMessage, len(envelope)+1)
	for k, v := range envelope {
		doc[k] = v
	}

	choice := sseOracleFinishChoice{
		Index:        choiceIdx,
		Delta:        sseOracleDeltaEmpty{},
		FinishReason: finishReason,
	}

	choicesJSON, err := jsonx.Marshal([]sseOracleFinishChoice{choice})
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
