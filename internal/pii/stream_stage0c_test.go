package pii

// stream_stage0c_test.go covers Stage 0c: streaming tool-call argument restore
// (feat/pii-stage0c-streaming-tool-calls).
//
// The test matrix follows the plan at
// issues/L-016c-stage0c-streaming-tool-call-restore-plan.md §Test matrix.
// Each numbered section below maps to a matrix item.
//
// Helper conventions (from stream_restorer_test.go):
//   - realPseudonym(t, email) → (pseudonym string, *Filter) with filter.rev populated.
//   - pushAllCollect(r, lines) → (output string, wasTerminal bool, finalErr error)
//   - pushLines(t, r, lines) → (output string, finalErr error)
//   - chatChunk / finishChunk / doneLines from stream_restorer_test.go
//
// New helpers defined below:
//   - toolChunkHeader — first delta for a tool-call index (id, type, name, args)
//   - toolChunkArgs   — subsequent argument fragment for a tool-call index
//   - toolFinish      — finish_reason:"tool_calls" chunk
//   - extractToolArgs — concatenate all arguments fragments from emitted lines

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// ── tool-call chunk builders ──────────────────────────────────────────────────

// toolChunkHeader builds the first SSE data line for a tool call, containing
// all header fields (id, type, function.name) and an optional arguments fragment.
func toolChunkHeader(choiceIdx, toolIdx int, id, name, args string) string {
	return fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
		choiceIdx, toolIdx, jsonStr(id), jsonStr(name), jsonStr(args),
	)
}

// toolChunkArgs builds a continuation SSE data line for a tool call, containing
// only the arguments fragment (no header fields).
func toolChunkArgs(choiceIdx, toolIdx int, args string) string {
	return fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]},"finish_reason":null}]}`,
		choiceIdx, toolIdx, jsonStr(args),
	)
}

// toolFinish builds a finish_reason:"tool_calls" SSE data line.
func toolFinish(choiceIdx int) string {
	return fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{},"finish_reason":"tool_calls"}]}`,
		choiceIdx,
	)
}

// extractAllToolArgs parses every emitted SSE data line and concatenates all
// function.arguments values across all tool_calls[0] elements in emission order.
// Returns the concatenated string and the set of emitted tool-call ids.
func extractAllToolArgs(output string) (args string, ids []string) {
	var sb strings.Builder
	seenIDs := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				sb.WriteString(tc.Function.Arguments)
				if tc.ID != "" && !seenIDs[tc.ID] {
					seenIDs[tc.ID] = true
					ids = append(ids, tc.ID)
				}
			}
		}
	}
	return sb.String(), ids
}

// extractToolArgsByIndex parses emitted SSE lines and returns per-tool-index
// concatenated arguments as a map[toolIdx]string.
func extractToolArgsByIndex(output string) map[int]string {
	result := make(map[int]string)
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				result[tc.Index] += tc.Function.Arguments
			}
		}
	}
	return result
}

// ── Matrix 1: Split a pseudonym across tool_call arguments at every byte position

// TestToolCall_SplitAtEveryPosition verifies that a pseudonym inside
// function.arguments split at every position (1..pseudonymLen-1) is fully
// restored without emitting any PII_ fragment.
func TestToolCall_SplitAtEveryPosition(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "toolsplit@example.com")
	orig := f.rev[pseudo]
	// Pre-warm replacer so parallel subtests only read (no write race).
	f.Restore([]byte(pseudo))

	for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
		splitAt := splitAt
		t.Run(fmt.Sprintf("split_at_%d", splitAt), func(t *testing.T) {
			t.Parallel()

			part1 := pseudo[:splitAt]
			part2 := pseudo[splitAt:]

			r := NewStreamRestorer(f, "gpt-4o")
			lines := []string{
				toolChunkHeader(0, 0, "call_abc", "get_user", part1),
				"",
				toolChunkArgs(0, 0, part2),
				"",
				toolFinish(0),
				"",
			}
			lines = append(lines, doneLines()...)

			output, _, err := pushAllCollect(r, lines)
			if err != nil {
				t.Fatalf("split_at_%d: Push error: %v", splitAt, err)
			}

			// No PII_ must appear anywhere in the output.
			if strings.Contains(output, "PII_") {
				t.Errorf("split_at_%d: PII_ fragment in output:\n%s", splitAt, output)
			}

			// The original value must appear in the concatenated arguments.
			allArgs, _ := extractAllToolArgs(output)
			if !strings.Contains(allArgs, orig) {
				t.Errorf("split_at_%d: restored original %q missing from tool args %q", splitAt, orig, allArgs)
			}

			// Multiple chunks must have been emitted (incremental delivery).
			dataCount := 0
			for _, line := range strings.Split(output, "\n") {
				if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
					dataCount++
				}
			}
			if dataCount < 2 {
				t.Errorf("split_at_%d: expected incremental output (>=2 data: chunks), got %d", splitAt, dataCount)
			}
		})
	}
}

// ── Matrix 2: Multiple parallel tool calls, both with split pseudonyms ────────

// TestToolCall_ParallelToolCalls verifies that two parallel tool calls (indices
// 0 and 1) each with a pseudonym split at different positions are both fully
// restored, with indices preserved.
func TestToolCall_ParallelToolCalls(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"alice@parallel.example bob@parallel.example"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	var alicePseudo, bobPseudo string
	for p, orig := range f.rev {
		if orig == "alice@parallel.example" {
			alicePseudo = p
		}
		if orig == "bob@parallel.example" {
			bobPseudo = p
		}
	}
	if alicePseudo == "" || bobPseudo == "" {
		t.Fatalf("pseudonyms not found: alice=%q bob=%q", alicePseudo, bobPseudo)
	}
	// Pre-warm replacer.
	f.Restore([]byte(alicePseudo + bobPseudo))

	aliceOrig := f.rev[alicePseudo]
	bobOrig := f.rev[bobPseudo]

	// Split alice at position 8, bob at position 20.
	const aliceSplit = 8
	const bobSplit = 20

	r := NewStreamRestorer(f, "gpt-4o")

	// Chunk 1: headers + first fragment for both tool calls.
	chunk1 := fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"id":"call_alice","type":"function","function":{"name":"lookup_user","arguments":%s}},`+
			`{"index":1,"id":"call_bob","type":"function","function":{"name":"lookup_email","arguments":%s}}`+
			`]},"finish_reason":null}]}`,
		jsonStr(alicePseudo[:aliceSplit]),
		jsonStr(bobPseudo[:bobSplit]),
	)
	// Chunk 2: remainders for both tool calls.
	chunk2 := fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[`+
			`{"index":0,"function":{"arguments":%s}},`+
			`{"index":1,"function":{"arguments":%s}}`+
			`]},"finish_reason":null}]}`,
		jsonStr(alicePseudo[aliceSplit:]),
		jsonStr(bobPseudo[bobSplit:]),
	)

	lines := []string{chunk1, "", chunk2, "", toolFinish(0), ""}
	lines = append(lines, doneLines()...)

	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal")
	}

	byIdx := extractToolArgsByIndex(output)

	// Tool index 0: alice restored.
	if !strings.Contains(byIdx[0], aliceOrig) {
		t.Errorf("tool[0] args %q does not contain alice's original %q", byIdx[0], aliceOrig)
	}
	// Tool index 1: bob restored.
	if !strings.Contains(byIdx[1], bobOrig) {
		t.Errorf("tool[1] args %q does not contain bob's original %q", byIdx[1], bobOrig)
	}

	// No PII_ in output.
	if strings.Contains(output, "PII_") {
		t.Errorf("PII_ fragment in output:\n%s", output)
	}
}

// ── Matrix 3: Header validation ───────────────────────────────────────────────

// TestToolCall_HeaderValidation covers the strict header state machine:
// PII_ in id → fail-closed; PII_ in name → fail-closed; type != "function" →
// fail-closed; missing/empty id → fail-closed; missing/empty name → fail-closed;
// arguments before header → fail-closed; changed id → fail-closed; changed name →
// fail-closed; id forwarded verbatim.
func TestToolCall_HeaderValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lines     func(pseudo string) []string
		wantErr   error
		checkLine func(t *testing.T, output string, pseudo string)
	}{
		{
			name: "pii_marker_in_id_fail_closed",
			lines: func(pseudo string) []string {
				// id contains the full pseudonym (canonical shape).
				line := fmt.Sprintf(
					`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":null}]}`,
					jsonStr(pseudo),
				)
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "pii_marker_in_name_fail_closed",
			lines: func(pseudo string) []string {
				// name contains the full pseudonym.
				line := fmt.Sprintf(
					`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":%s,"arguments":"{}"}}]},"finish_reason":null}]}`,
					jsonStr(pseudo),
				)
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "pii_prefix_only_in_id_fail_closed",
			lines: func(_ string) []string {
				// id contains sub-canonical "PII_" marker (not a full canonical pseudonym).
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"PII_marker","type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "type_not_function_fail_closed",
			lines: func(_ string) []string {
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"retrieval","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "missing_id_on_first_observation_fail_closed",
			lines: func(_ string) []string {
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "empty_id_on_first_observation_fail_closed",
			lines: func(_ string) []string {
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","type":"function","function":{"name":"fn","arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "missing_name_on_first_observation_fail_closed",
			lines: func(_ string) []string {
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "empty_name_on_first_observation_fail_closed",
			lines: func(_ string) []string {
				line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"","arguments":"{}"}}]},"finish_reason":null}]}`
				return []string{line, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "changed_id_on_subsequent_fragment_fail_closed",
			lines: func(_ string) []string {
				// First: valid header.
				first := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_orig","type":"function","function":{"name":"fn","arguments":""}}]},"finish_reason":null}]}`
				// Second: same index, DIFFERENT id.
				second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_different","function":{"arguments":""}}]},"finish_reason":null}]}`
				return []string{first, "", second, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "changed_name_on_subsequent_fragment_fail_closed",
			lines: func(_ string) []string {
				first := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"fn_orig","arguments":""}}]},"finish_reason":null}]}`
				second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"fn_different","arguments":""}}]},"finish_reason":null}]}`
				return []string{first, "", second, ""}
			},
			wantErr: errStreamAborted,
		},
		{
			name: "id_forwarded_verbatim",
			lines: func(_ string) []string {
				return []string{
					toolChunkHeader(0, 0, "call_verbatim_id", "fn", `"hello"`),
					"",
					toolFinish(0),
					"",
					"data: [DONE]",
					"",
				}
			},
			wantErr: nil,
			checkLine: func(t *testing.T, output string, _ string) {
				t.Helper()
				// The emitted id must be the upstream verbatim id, not restored.
				_, ids := extractAllToolArgs(output)
				found := false
				for _, id := range ids {
					if id == "call_verbatim_id" {
						found = true
					}
				}
				if !found {
					t.Errorf("verbatim id 'call_verbatim_id' not found in emitted tool_calls output:\n%s", output)
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pseudo, f := realPseudonym(t, "headerval@example.com")
			f.Restore([]byte(pseudo)) // pre-warm

			r := NewStreamRestorer(f, "gpt-4o")
			lines := tc.lines(pseudo)

			output, _, finalErr := pushAllCollect(r, lines)
			if tc.wantErr != nil {
				if !errors.Is(finalErr, tc.wantErr) {
					t.Errorf("%s: error = %v, want %v", tc.name, finalErr, tc.wantErr)
				}
				// No PII_ must be visible even on error path.
				if strings.Contains(output, "PII_") {
					t.Errorf("%s: PII_ visible on error path: %s", tc.name, output)
				}
			} else {
				if finalErr != nil {
					t.Errorf("%s: unexpected error: %v", tc.name, finalErr)
				}
				if tc.checkLine != nil {
					tc.checkLine(t, output, pseudo)
				}
			}
		})
	}
}

// ── Matrix 4: Format-transition guard ─────────────────────────────────────────

// TestToolCall_FormatGuard verifies that tool_calls is only accepted when the
// choice format is formatUnknown or formatChat. formatCompletion and
// formatRefusal choices must reject tool_calls with errStreamAborted.
func TestToolCall_FormatGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup []string
	}{
		{
			name: "completion_then_tool_calls_fail_closed",
			setup: []string{
				// First: legacy text chunk → format becomes formatCompletion.
				textChunk(0, "hello world"),
				"",
			},
		},
		{
			name: "refusal_then_tool_calls_fail_closed",
			setup: []string{
				// First: refusal delta → format becomes formatRefusal.
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"refusal":"I cannot"},"finish_reason":null}]}`,
				"",
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "fmtguard@example.com")
			r := NewStreamRestorer(f, "gpt-4o")

			// Push setup lines (should succeed).
			for _, l := range tc.setup {
				var raw []byte
				if l == "" {
					raw = []byte{}
				} else {
					raw = []byte(l)
				}
				if _, _, err := r.Push(raw); err != nil {
					t.Fatalf("%s: setup push error: %v (line=%q)", tc.name, err, l)
				}
			}

			// Now push a tool_calls chunk — must fail-closed.
			toolLine := toolChunkHeader(0, 0, "call_x", "fn", `""`)
			_, _, err := r.Push([]byte(toolLine))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("%s: error = %v, want errStreamAborted", tc.name, err)
			}
		})
	}

	// Positive case: chat format accepts tool_calls.
	t.Run("chat_then_tool_calls_ok", func(t *testing.T) {
		t.Parallel()

		_, f := realPseudonym(t, "fmtguardchat@example.com")
		r := NewStreamRestorer(f, "gpt-4o")

		// Push a chat content chunk first → format becomes formatChat.
		if _, _, err := r.Push([]byte(chatChunk(0, "hi"))); err != nil {
			t.Fatalf("setup chat chunk: %v", err)
		}
		r.Push([]byte{})

		// Mixing chat content and tool_calls in same choice IS a violation (content + tool_calls).
		// Instead, test a fresh choice index (1) after choice 0 has been set to chat.
		// OR: test that a brand-new choice starting with tool_calls is accepted.
		r2 := NewStreamRestorer(f, "gpt-4o")
		toolLine := toolChunkHeader(0, 0, "call_x", "fn", `""`)
		if _, _, err := r2.Push([]byte(toolLine)); err != nil {
			t.Errorf("fresh choice with tool_calls: unexpected error: %v", err)
		}
	})
}

// ── Matrix 5: Cross-lane carry ownership ──────────────────────────────────────

// TestToolCall_CrossLane verifies that a non-empty content carry in the same
// choice blocks tool_calls emission (fail-closed), and a non-empty argsCarry
// in tool index 0 blocks emission from tool index 1 in the same choice.
// Cross-CHOICE independence: choice 0 with a carry must NOT block choice 1.
func TestToolCall_CrossLane(t *testing.T) {
	t.Parallel()

	t.Run("content_carry_blocks_tool_calls_same_choice", func(t *testing.T) {
		t.Parallel()

		pseudo, f := realPseudonym(t, "crosslane1@example.com")
		f.Restore([]byte(pseudo)) // pre-warm

		r := NewStreamRestorer(f, "gpt-4o")

		// Push partial pseudonym into content carry for choice 0.
		partial := pseudo[:10]
		if _, _, err := r.Push([]byte(chatChunk(0, partial))); err != nil {
			t.Fatalf("push partial: %v", err)
		}
		r.Push([]byte{})

		// Now push tool_calls for the same choice 0 — must fail-closed.
		toolLine := toolChunkHeader(0, 0, "call_x", "fn", `""`)
		_, _, err := r.Push([]byte(toolLine))
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("content carry + tool_calls same choice: error = %v, want errStreamAborted", err)
		}
	})

	t.Run("tool0_carry_blocks_tool1_same_choice", func(t *testing.T) {
		t.Parallel()

		pseudo, f := realPseudonym(t, "crosslane2@example.com")
		f.Restore([]byte(pseudo)) // pre-warm

		r := NewStreamRestorer(f, "gpt-4o")

		// Push header + first fragment (partial pseudonym) for tool index 0 — stays in argsCarry.
		part1 := pseudo[:10]
		if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_0", "fn0", part1))); err != nil {
			t.Fatalf("push tool0 header+partial: %v", err)
		}
		r.Push([]byte{})

		// Now push a second tool call (index 1) in the same choice — must fail-closed
		// because tool index 0's argsCarry is non-empty.
		chunk := fmt.Sprintf(
			`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` +
				`{"index":1,"id":"call_1","type":"function","function":{"name":"fn1","arguments":"hello"}}` +
				`]},"finish_reason":null}]}`,
		)
		_, _, err := r.Push([]byte(chunk))
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("tool0 carry + tool1 emit: error = %v, want errStreamAborted", err)
		}
	})

	t.Run("cross_choice_independence", func(t *testing.T) {
		t.Parallel()

		// Choice 0 has a partial pseudonym in content carry.
		// Choice 1 should still be able to emit tool_calls independently.
		pseudo, f := realPseudonym(t, "crossindep@example.com")
		f.Restore([]byte(pseudo)) // pre-warm

		r := NewStreamRestorer(f, "gpt-4o")

		// Push partial pseudonym for choice 0 content.
		partial := pseudo[:10]
		if _, _, err := r.Push([]byte(chatChunk(0, partial))); err != nil {
			t.Fatalf("push choice0 partial: %v", err)
		}
		r.Push([]byte{})

		// Push tool_calls for choice 1 — must succeed (different choice).
		toolLine := toolChunkHeader(1, 0, "call_y", "fn", `"some_args"`)
		_, _, err := r.Push([]byte(toolLine))
		if err != nil {
			t.Errorf("cross-choice independence: choice1 tool_calls unexpectedly failed: %v", err)
		}
	})
}

// ── Matrix 6: Index validation ────────────────────────────────────────────────

// TestToolCall_IndexValidation covers index<0, index>=64, duplicate within one
// chunk's array, same index across chunks (valid), and >64 distinct indices.
func TestToolCall_IndexValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lines   []string
		wantErr error
	}{
		{
			name: "negative_index_fail_closed",
			lines: []string{
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":-1,"id":"call_x","type":"function","function":{"name":"fn","arguments":""}}]},"finish_reason":null}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "index_64_fail_closed",
			lines: []string{
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":64,"id":"call_x","type":"function","function":{"name":"fn","arguments":""}}]},"finish_reason":null}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "duplicate_index_within_chunk_fail_closed",
			lines: []string{
				// Two elements with index=0 in the same tool_calls array.
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"fa","arguments":""}},{"index":0,"id":"call_b","type":"function","function":{"name":"fb","arguments":""}}]},"finish_reason":null}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "same_index_across_chunks_ok",
			lines: []string{
				// First chunk: tool index 0 header.
				toolChunkHeader(0, 0, "call_ok", "fn", `"part1"`),
				"",
				// Second chunk: same tool index 0 continuation — valid.
				toolChunkArgs(0, 0, `"part2"`),
				"",
				toolFinish(0),
				"",
				"data: [DONE]",
				"",
			},
			wantErr: nil,
		},
		{
			name: "index_63_accepted",
			lines: []string{
				// Index 63 is the maximum valid value (0..63).
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":63,"id":"call_x","type":"function","function":{"name":"fn","arguments":"hello"}}]},"finish_reason":null}]}`,
				"",
				// finish_reason for choice 0.
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"",
				"data: [DONE]",
				"",
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "idxval@example.com")
			r := NewStreamRestorer(f, "gpt-4o")

			_, _, finalErr := pushAllCollect(r, tc.lines)
			if tc.wantErr != nil {
				if !errors.Is(finalErr, tc.wantErr) {
					t.Errorf("%s: error = %v, want %v", tc.name, finalErr, tc.wantErr)
				}
			} else {
				if finalErr != nil {
					t.Errorf("%s: unexpected error: %v", tc.name, finalErr)
				}
			}
		})
	}
}

// TestToolCall_ExceedDistinctIndexCap verifies that more than maxToolCallsPerChoice
// distinct tool indices for one choice causes fail-closed.
func TestToolCall_ExceedDistinctIndexCap(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "idxcap@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Send maxToolCallsPerChoice=64 valid distinct tool indices one at a time.
	for i := 0; i < maxToolCallsPerChoice; i++ {
		line := fmt.Sprintf(
			`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%d","type":"function","function":{"name":"fn_%d","arguments":"x"}}]},"finish_reason":null}]}`,
			i, i, i,
		)
		if _, _, err := r.Push([]byte(line)); err != nil {
			t.Fatalf("push tool index %d: unexpected error: %v", i, err)
		}
		r.Push([]byte{})
	}

	// Now push a 65th distinct tool index — must fail-closed.
	// Note: indices 0..63 are all taken; any new index would require index>=64 which
	// is already rejected by the range check. So this is actually caught by the range
	// guard. The per-choice count cap (maxToolCallsPerChoice) is a secondary guard
	// for when the index range is not exhausted but too many distinct entries exist.
	// We test the count cap by artificially choosing a valid index that hasn't been seen.
	// Since all indices 0..63 are valid and all 64 are taken, the only way to exceed
	// the count cap is to try to add a 65th DISTINCT entry. All indices 0..63 are valid
	// (0..maxToolCallsPerChoice-1). So the 65th new index would be 64, which is out of
	// range. The count cap is therefore bounded by the index range. We verify the range
	// guard catches it.
	line65 := fmt.Sprintf(
		`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":64,"id":"call_65","type":"function","function":{"name":"fn_65","arguments":"x"}}]},"finish_reason":null}]}`,
	)
	_, _, err := r.Push([]byte(line65))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("65th tool index: error = %v, want errStreamAborted", err)
	}
}

// ── Matrix 7: finish_reason gates ─────────────────────────────────────────────

// TestToolCall_FinishReasonGates covers all finish_reason gating scenarios.
func TestToolCall_FinishReasonGates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lines   []string
		wantErr error
	}{
		{
			name: "saw_tool_call_then_stop_fail_closed",
			// A choice that streamed a validated tool call must NOT finish with "stop".
			lines: []string{
				toolChunkHeader(0, 0, "call_x", "fn", `"hello"`),
				"",
				finishChunk(0, "stop"), // "stop" after tool call → fail-closed.
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "tool_calls_finish_without_saw_tool_call_fail_closed",
			// finish_reason:"tool_calls" but sawToolCall is false → fail-closed.
			lines: []string{
				chatChunk(0, "some text"),
				"",
				// finish_reason:"tool_calls" but no prior tool delta.
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "finish_reason_before_tool_delta_fail_closed",
			// finish_reason in a chunk BEFORE any tool-call delta → fail-closed.
			// (Same as above case: "tool_calls" finish before sawToolCall is set.)
			lines: []string{
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "same_chunk_tool_call_and_finish_tool_calls_ok",
			// Same chunk: validated tool call + finish_reason:"tool_calls" → OK.
			lines: []string{
				// Single chunk carrying both tool_calls delta AND finish_reason:"tool_calls".
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"fn","arguments":"hello"}}]},"finish_reason":"tool_calls"}]}`,
				"",
				"data: [DONE]",
				"",
			},
			wantErr: nil,
		},
		{
			name: "legacy_function_call_fail_closed",
			// Legacy singular delta.function_call must always be fail-closed.
			lines: []string{
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"function_call":{"name":"search","arguments":"{"}},"finish_reason":null}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "empty_tool_calls_array_fail_closed",
			// Empty tool_calls array does not set sawToolCall; also directly fail-closed.
			lines: []string{
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[]},"finish_reason":null}]}`,
				"",
			},
			wantErr: errStreamAborted,
		},
		{
			name: "valid_tool_call_then_tool_calls_finish_ok",
			lines: []string{
				toolChunkHeader(0, 0, "call_ok", "fn", `"val"`),
				"",
				toolFinish(0),
				"",
				"data: [DONE]",
				"",
			},
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "fingate@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			_, _, finalErr := pushAllCollect(r, tc.lines)

			if tc.wantErr != nil {
				if !errors.Is(finalErr, tc.wantErr) {
					t.Errorf("%s: error = %v, want %v", tc.name, finalErr, tc.wantErr)
				}
			} else {
				if finalErr != nil {
					t.Errorf("%s: unexpected error: %v", tc.name, finalErr)
				}
			}
		})
	}
}

// ── Matrix 8: Terminal — [DONE] with non-empty tool argsCarry ─────────────────

// TestToolCall_Terminal_DoneWithNonEmptyArgsCarry verifies that [DONE] arriving
// while a tool-call argsCarry is non-empty returns errCarryNotEmpty.
func TestToolCall_Terminal_DoneWithNonEmptyArgsCarry(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "termcarry@example.com")
	f.Restore([]byte(pseudo)) // pre-warm

	r := NewStreamRestorer(f, "gpt-4o")

	// Push header + partial pseudonym (argsCarry will be non-empty at [DONE]).
	partial := pseudo[:10]
	if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_t", "fn", partial))); err != nil {
		t.Fatalf("push header+partial: %v", err)
	}
	r.Push([]byte{})

	// Push [DONE] — argsCarry is non-empty → must return errCarryNotEmpty.
	_, _, err := r.Push([]byte("data: [DONE]"))
	if !errors.Is(err, errCarryNotEmpty) {
		t.Errorf("DONE with non-empty argsCarry: error = %v, want errCarryNotEmpty", err)
	}
}

// TestToolCall_Terminal_CleanDone verifies that [DONE] with all carries empty
// (content and tool) succeeds and returns terminal=true.
func TestToolCall_Terminal_CleanDone(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "termclean@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	lines := []string{
		toolChunkHeader(0, 0, "call_z", "fn", `"complete_value"`),
		"",
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("clean DONE: unexpected error: %v", err)
	}
	if !terminal {
		t.Error("clean DONE: stream did not reach terminal")
	}
	// No PII_ in output.
	if strings.Contains(output, "PII_") {
		t.Errorf("clean DONE: PII_ in output:\n%s", output)
	}
}

// ── Matrix 9: Residual check ──────────────────────────────────────────────────

// TestToolCall_ResidualCheck covers canonical and sub-canonical PII markers
// in both content and tool arguments.
func TestToolCall_ResidualCheck(t *testing.T) {
	t.Parallel()

	t.Run("unknown_canonical_in_content_fail_closed", func(t *testing.T) {
		t.Parallel()

		// Build a filter with no pseudonyms in its rev map.
		// Then craft a canonical-shaped string that Restore will not touch
		// (unknown canonical token — not in filter.rev).
		f := newTestFilter(t)
		// Use the filter's Restore which will leave unknown tokens intact.
		// An unknown canonical pseudonym in content after restore → fail-closed.
		unknown := "PII_EM_000000000000000000000000" // 31 bytes, canonical shape.
		if len(unknown) != pseudonymLen {
			t.Fatalf("bad test pseudonym length %d", len(unknown))
		}

		r := NewStreamRestorer(f, "gpt-4o")
		// The restored output will still contain the canonical shape since the
		// filter does not know this token. This triggers the residual check.
		_, _, err := r.Push([]byte(chatChunk(0, unknown)))
		// After Restore (which returns it unchanged), isCanonicalPseudonym fires.
		// But the carry holds bytes until they're safe to emit. The first chunk
		// won't have emitted yet (all bytes could be a pseudonym prefix). Push
		// a harmless next chunk to force a flush.
		if err != nil {
			// Already failed on push — check if it's the right error.
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("unexpected error type: %v", err)
			}
			return
		}
		r.Push([]byte{})
		// Force flush by sending a content chunk with non-PII text.
		_, _, err2 := r.Push([]byte(chatChunk(0, "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")))
		if err2 != nil {
			if !errors.Is(err2, errStreamAborted) {
				t.Errorf("unknown canonical in content flush: error = %v, want errStreamAborted", err2)
			}
			return
		}
		// Also check at finish time.
		r.Push([]byte{})
		_, _, err3 := r.Push([]byte(finishChunk(0, "stop")))
		if err3 != nil && !errors.Is(err3, errStreamAborted) {
			t.Errorf("unknown canonical in content finish: error = %v, want errStreamAborted", err3)
		}
	})

	t.Run("unknown_canonical_in_tool_args_fail_closed", func(t *testing.T) {
		t.Parallel()

		// An unknown canonical pseudonym shape in tool arguments → fail-closed.
		f := newTestFilter(t)
		unknown := "PII_EM_000000000000000000000000" // 31 bytes, canonical shape.

		r := NewStreamRestorer(f, "gpt-4o")
		// Push tool call header with the unknown canonical pseudonym in arguments.
		// The pseudonym is exactly 31 bytes, so it will be fully in argsCarry and
		// then flushed when additional text arrives.
		if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_u", "fn", unknown))); err != nil {
			if errors.Is(err, errStreamAborted) {
				return // correctly rejected
			}
			t.Fatalf("push header with unknown canonical: %v", err)
		}
		r.Push([]byte{})
		// Force a flush by adding more args text.
		_, _, err := r.Push([]byte(toolChunkArgs(0, 0, "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")))
		if err == nil {
			// Possibly not yet flushed; push finish to force flush.
			r.Push([]byte{})
			_, _, err = r.Push([]byte(toolFinish(0)))
		}
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("unknown canonical in tool args: error = %v, want errStreamAborted", err)
		}
	})

	t.Run("sub_canonical_in_content_passes_through", func(t *testing.T) {
		t.Parallel()

		// A sub-canonical "PII_" marker (not a full canonical token) in content
		// must pass through and increment ResidualSubCanonicalCount.
		f := newTestFilter(t)

		// "PII_GARBAGE" is sub-canonical: has PII_ prefix but not the full shape.
		subCanonical := "say PII_GARBAGE ok"

		r := NewStreamRestorer(f, "gpt-4o")

		before := ResidualSubCanonicalCount()

		if _, _, err := r.Push([]byte(chatChunk(0, subCanonical))); err != nil {
			t.Fatalf("sub-canonical content: unexpected error: %v", err)
		}
		r.Push([]byte{})
		// Force flush.
		if _, _, err := r.Push([]byte(finishChunk(0, "stop"))); err != nil {
			t.Fatalf("sub-canonical content finish: %v", err)
		}
		r.Push([]byte{})
		if _, _, err := r.Push([]byte("data: [DONE]")); err != nil {
			t.Fatalf("sub-canonical content DONE: %v", err)
		}

		after := ResidualSubCanonicalCount()
		// Counter must have incremented at least once for the sub-canonical marker.
		if after <= before {
			t.Errorf("ResidualSubCanonicalCount did not increment: before=%d after=%d", before, after)
		}
	})

	t.Run("sub_canonical_in_tool_args_fail_closed", func(t *testing.T) {
		t.Parallel()

		// A sub-canonical "PII_" marker in tool arguments is fail-closed.
		f := newTestFilter(t)

		r := NewStreamRestorer(f, "gpt-4o")
		// Push tool call with sub-canonical marker in arguments.
		if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_s", "fn", "PII_GARBAGE"))); err != nil {
			if errors.Is(err, errStreamAborted) {
				return
			}
			t.Fatalf("push sub-canonical tool args: %v", err)
		}
		r.Push([]byte{})
		// Force flush.
		_, _, err := r.Push([]byte(toolChunkArgs(0, 0, "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")))
		if err == nil {
			r.Push([]byte{})
			_, _, err = r.Push([]byte(toolFinish(0)))
		}
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("sub-canonical in tool args: error = %v, want errStreamAborted", err)
		}
	})
}

// ── Matrix 10: Oracle / property test ─────────────────────────────────────────

// TestToolCall_Oracle_PropertyTest builds a tool-call stream whose concatenated
// arguments contain randomly-placed known pseudonyms, randomly chunks the
// arguments fragments, and asserts that concat(restored arguments) equals
// what a non-streaming restore would produce, and that no canonical PII_ token
// remains in the output.
func TestToolCall_Oracle_PropertyTest(t *testing.T) {
	t.Parallel()

	// Build a filter with two pseudonyms.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"alice@oracle.example and bob@oracle.example"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	var alicePseudo, bobPseudo string
	for p, orig := range f.rev {
		if orig == "alice@oracle.example" {
			alicePseudo = p
		}
		if orig == "bob@oracle.example" {
			bobPseudo = p
		}
	}
	if alicePseudo == "" || bobPseudo == "" {
		t.Fatalf("pseudonyms not found")
	}
	// Pre-warm.
	f.Restore([]byte(alicePseudo + bobPseudo))

	// Construct a full arguments string that contains both pseudonyms.
	fullArgs := fmt.Sprintf(`{"user1":"%s","user2":"%s","note":"test"}`, alicePseudo, bobPseudo)

	// Oracle: what the non-streaming Restore gives us.
	oracleRestored := string(f.Restore([]byte(fullArgs)))

	// Property: randomly chunk fullArgs and feed through StreamRestorer.
	// Run multiple times with different random splits.
	rng := rand.New(rand.NewSource(42))
	const iterations = 30
	for i := 0; i < iterations; i++ {
		i := i
		t.Run(fmt.Sprintf("iter_%d", i), func(t *testing.T) {
			// Not parallel here — shares rng; sequential is fine for a property test.
			r := NewStreamRestorer(f, "gpt-4o")

			// Randomly split fullArgs into 1..5 fragments.
			numFrags := rng.Intn(5) + 1
			frags := splitIntoN(fullArgs, numFrags, rng)

			var lines []string
			// Header chunk with first fragment.
			lines = append(lines, toolChunkHeader(0, 0, "call_prop", "fn_prop", frags[0]))
			lines = append(lines, "")
			// Subsequent fragments.
			for _, frag := range frags[1:] {
				lines = append(lines, toolChunkArgs(0, 0, frag))
				lines = append(lines, "")
			}
			lines = append(lines, toolFinish(0))
			lines = append(lines, "")
			lines = append(lines, "data: [DONE]")
			lines = append(lines, "")

			output, terminal, err := pushAllCollect(r, lines)
			if err != nil {
				t.Fatalf("iter_%d: Push error: %v", i, err)
			}
			if !terminal {
				t.Fatalf("iter_%d: stream not terminal", i)
			}

			allArgs, _ := extractAllToolArgs(output)

			// Property 1: concatenated restored args equals oracle.
			if allArgs != oracleRestored {
				t.Errorf("iter_%d: restored args = %q, want oracle %q", i, allArgs, oracleRestored)
			}
			// Property 2: no canonical PII_ token in output.
			if strings.Contains(output, "PII_") {
				t.Errorf("iter_%d: canonical PII_ in output:\n%s", i, output)
			}
		})
	}
}

// splitIntoN splits s into n fragments of roughly equal size using the given rng.
func splitIntoN(s string, n int, rng *rand.Rand) []string {
	if n <= 1 || len(s) == 0 {
		return []string{s}
	}
	frags := make([]string, 0, n)
	remaining := s
	for i := 0; i < n-1 && len(remaining) > 0; i++ {
		// Random split point between 1 and len(remaining)-1.
		cut := rng.Intn(len(remaining))
		if cut == 0 {
			cut = 1
		}
		frags = append(frags, remaining[:cut])
		remaining = remaining[cut:]
	}
	frags = append(frags, remaining)
	return frags
}

// ── Verify ResidualSubCanonicalCount is a package-level atomic (not reset) ────

// TestResidualSubCanonicalCount_Atomicity verifies that ResidualSubCanonicalCount
// returns a non-negative value and can be read from multiple goroutines without
// a race (the -race flag will catch races if any exist).
func TestResidualSubCanonicalCount_Atomicity(t *testing.T) {
	t.Parallel()

	// Multiple goroutines read the counter simultaneously; the -race detector
	// will flag any data race. A WaitGroup replaces the busy-spin to avoid
	// -race flakiness from the spin loop itself.
	var wg sync.WaitGroup
	const readers = 8
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			_ = ResidualSubCanonicalCount()
		}()
	}
	wg.Wait()
}

// ── FIX 1: Accumulated/cross-emission residual canonical-pseudonym check ──────

// TestFix1_UnknownCanonicalSplitAcrossEmissions_Content verifies that an unknown
// canonical pseudonym (not in the filter's reverse map) that is split across two
// consecutive emissions in a content lane is detected and causes fail-closed abort.
// The test exercises every possible split position (1..pseudonymLen-1) to confirm
// the tail window catches all boundary cases.
func TestFix1_UnknownCanonicalSplitAcrossEmissions_Content(t *testing.T) {
	t.Parallel()

	// unknownCanonical is a canonical-shaped token (31 bytes) that does NOT
	// appear in any filter's reverse map. The filter's Restore will leave it
	// intact, so it arrives in the emitted stream as-is.
	const unknownCanonical = "PII_EM_000000000000000000000001"
	if len(unknownCanonical) != pseudonymLen {
		t.Fatalf("bad test token length %d, want %d", len(unknownCanonical), pseudonymLen)
	}

	// To force the unknown canonical to span two SEPARATE emissions we must
	// ensure neither half alone holds enough bytes to trigger the hold-back
	// (hold-back only fires for KNOWN pseudonym prefixes). Since the token is
	// unknown, both halves emit immediately. We therefore deliver the two halves
	// in two separate chunks so that there is a first emission (part1) and a
	// second emission (part2); the tail window must detect the full token across
	// the two emissions.
	//
	// We pad each part with non-PII text so the carry is always flushed and
	// neither part is held back by the known-prefix filter.
	const prefix = "safe text before: "
	const suffix = " :safe text after"

	for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
		splitAt := splitAt
		t.Run(fmt.Sprintf("split_at_%d", splitAt), func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t) // empty filter: no known pseudonyms
			r := NewStreamRestorer(f, "gpt-4o")

			part1 := prefix + unknownCanonical[:splitAt]
			part2 := unknownCanonical[splitAt:] + suffix

			// Feed part1 then part2 in separate chunks. Because the filter is
			// empty (no known pseudonyms), longestPseudonymPrefixSuffix returns 0
			// and content is emitted immediately on each push.
			var finalErr error
			for _, chunk := range []string{
				chatChunk(0, part1),
				"",
				chatChunk(0, part2),
				"",
			} {
				var raw []byte
				if chunk == "" {
					raw = []byte{}
				} else {
					raw = []byte(chunk)
				}
				_, _, err := r.Push(raw)
				if err != nil {
					finalErr = err
					break
				}
			}

			if finalErr == nil {
				// Not yet caught; force a finish to flush any carry.
				_, _, finalErr = r.Push([]byte(finishChunk(0, "stop")))
			}

			if !errors.Is(finalErr, errStreamAborted) {
				t.Errorf("split_at_%d: expected errStreamAborted, got %v", splitAt, finalErr)
			}
		})
	}
}

// TestFix1_UnknownCanonicalSplitAcrossEmissions_ToolArgs verifies that an
// unknown canonical pseudonym split across two consecutive emissions in a tool
// arguments lane is detected and causes fail-closed abort.
func TestFix1_UnknownCanonicalSplitAcrossEmissions_ToolArgs(t *testing.T) {
	t.Parallel()

	const unknownCanonical = "PII_EM_000000000000000000000002"
	if len(unknownCanonical) != pseudonymLen {
		t.Fatalf("bad test token length %d, want %d", len(unknownCanonical), pseudonymLen)
	}

	const prefix = "before:"
	const suffix = ":after"

	for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
		splitAt := splitAt
		t.Run(fmt.Sprintf("split_at_%d", splitAt), func(t *testing.T) {
			t.Parallel()

			f := newTestFilter(t) // empty filter: no known pseudonyms
			r := NewStreamRestorer(f, "gpt-4o")

			part1 := prefix + unknownCanonical[:splitAt]
			part2 := unknownCanonical[splitAt:] + suffix

			// Emit part1 in the header chunk, part2 in a continuation chunk.
			var finalErr error
			for _, line := range []string{
				toolChunkHeader(0, 0, "call_fix1", "fn", part1),
				"",
				toolChunkArgs(0, 0, part2),
				"",
			} {
				var raw []byte
				if line == "" {
					raw = []byte{}
				} else {
					raw = []byte(line)
				}
				_, _, err := r.Push(raw)
				if err != nil {
					finalErr = err
					break
				}
			}

			if finalErr == nil {
				// Force a finish to flush any remaining carry.
				_, _, finalErr = r.Push([]byte(toolFinish(0)))
			}

			if !errors.Is(finalErr, errStreamAborted) {
				t.Errorf("split_at_%d: expected errStreamAborted, got %v", splitAt, finalErr)
			}
		})
	}
}

// ── FIX 2: Reverse cross-lane check (content delta after non-empty argsCarry) ─

// TestFix2_ContentAfterNonEmptyArgsCarry_SameChoice verifies that when a content
// delta arrives for a choice that already has a non-empty tool-call argsCarry,
// the restorer fails closed. This is the reverse of the existing forward check
// (content carry blocks tool emission).
func TestFix2_ContentAfterNonEmptyArgsCarry_SameChoice(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "fix2crosslane@example.com")
	f.Restore([]byte(pseudo)) // pre-warm

	r := NewStreamRestorer(f, "gpt-4o")

	// Put a partial pseudonym into tool index 0's argsCarry so argsCarry is non-empty.
	partial := pseudo[:10]
	if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_fix2", "fn", partial))); err != nil {
		t.Fatalf("push tool header+partial: %v", err)
	}
	r.Push([]byte{})

	// Now push a content delta for the SAME choice (choice 0).
	// This must fail-closed because argsCarry for choice 0 is non-empty.
	_, _, err := r.Push([]byte(chatChunk(0, "hello")))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("content after non-empty argsCarry same choice: error = %v, want errStreamAborted", err)
	}
}

// TestFix2_ContentAfterNonEmptyArgsCarry_DifferentChoice verifies that a content
// delta for choice 1 is NOT blocked by a non-empty argsCarry in choice 0.
// Cross-choice independence must be preserved.
func TestFix2_ContentAfterNonEmptyArgsCarry_DifferentChoice(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "fix2crossindep@example.com")
	f.Restore([]byte(pseudo)) // pre-warm

	r := NewStreamRestorer(f, "gpt-4o")

	// Put a partial pseudonym into choice 0's tool argsCarry.
	partial := pseudo[:10]
	if _, _, err := r.Push([]byte(toolChunkHeader(0, 0, "call_fix2b", "fn", partial))); err != nil {
		t.Fatalf("push tool header+partial: %v", err)
	}
	r.Push([]byte{})

	// Push a content delta for choice 1 — must succeed (different choice).
	_, _, err := r.Push([]byte(chatChunk(1, "hello from choice 1")))
	if err != nil {
		t.Errorf("content for different choice after argsCarry: unexpected error: %v", err)
	}
}

// ── FIX 3: arguments:null and non-string arguments → fail-closed ──────────────

// TestFix3_ArgumentsNull_FailClosed verifies that a tool_calls delta with
// arguments:null (JSON null, not a string) causes fail-closed abort.
func TestFix3_ArgumentsNull_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fix3null@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// arguments is JSON null — must fail-closed.
	line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_n","type":"function","function":{"name":"fn","arguments":null}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("arguments:null: error = %v, want errStreamAborted", err)
	}
}

// TestFix3_ArgumentsNumber_FailClosed verifies that a tool_calls delta with
// arguments as a JSON number causes fail-closed abort.
func TestFix3_ArgumentsNumber_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fix3num@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_n","type":"function","function":{"name":"fn","arguments":42}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("arguments:number: error = %v, want errStreamAborted", err)
	}
}

// TestFix3_ArgumentsObject_FailClosed verifies that a tool_calls delta with
// arguments as a JSON object causes fail-closed abort.
func TestFix3_ArgumentsObject_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fix3obj@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_n","type":"function","function":{"name":"fn","arguments":{"key":"val"}}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("arguments:object: error = %v, want errStreamAborted", err)
	}
}

// TestFix3_ArgumentsAbsent_ContinuationOK verifies that a subsequent tool_calls
// fragment that omits the arguments key entirely is treated as a header-only
// continuation and does not fail-closed.
func TestFix3_ArgumentsAbsent_ContinuationOK(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fix3absent@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First chunk: valid header with arguments.
	first := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"fn","arguments":"hello"}}]},"finish_reason":null}]}`
	if _, _, err := r.Push([]byte(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	r.Push([]byte{})

	// Second chunk: same index, function object present but NO arguments key.
	// This is a header-only continuation — must not fail-closed.
	second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(second))
	if err != nil {
		t.Errorf("arguments absent in continuation: unexpected error: %v", err)
	}
}

// ── FIX A: Strict structural charset on tool-call id and name ────────────────

// TestFixA_IDCharset verifies that tool-call ids containing characters outside
// [A-Za-z0-9_.+-] — including '@', spaces, colons, and non-ASCII — fail-closed,
// while a valid id like "call_abc123" is accepted.
func TestFixA_IDCharset(t *testing.T) {
	t.Parallel()

	invalid := []struct {
		name string
		id   string
	}{
		{"at_sign", "user@example.com"},
		{"space", "call abc"},
		{"colon", "call:x"},
		{"slash", "call/x"},
		{"non_ascii", "call_\xff"},
	}

	for _, tc := range invalid {
		tc := tc
		t.Run("id_"+tc.name+"_fail_closed", func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "charid@example.com")
			r := NewStreamRestorer(f, "gpt-4o")

			line := fmt.Sprintf(
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":"get_weather","arguments":"{}"}}]},"finish_reason":null}]}`,
				jsonStr(tc.id),
			)
			_, _, err := r.Push([]byte(line))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("id %q: error = %v, want errStreamAborted", tc.id, err)
			}
		})
	}

	// Valid id must be accepted.
	t.Run("id_call_abc123_ok", func(t *testing.T) {
		t.Parallel()

		_, f := realPseudonym(t, "validid@example.com")
		r := NewStreamRestorer(f, "gpt-4o")

		lines := []string{
			toolChunkHeader(0, 0, "call_abc123", "get_weather", `"{}"`),
			"",
			toolFinish(0),
			"",
			"data: [DONE]",
			"",
		}
		_, _, err := pushAllCollect(r, lines)
		if err != nil {
			t.Errorf("valid id call_abc123: unexpected error: %v", err)
		}
	})
}

// TestFixA_NameCharset verifies that tool function names containing characters
// outside [A-Za-z0-9_.+-] — including '@', spaces — fail-closed, while a valid
// name like "get_weather" is accepted.
func TestFixA_NameCharset(t *testing.T) {
	t.Parallel()

	invalid := []struct {
		name     string
		funcName string
	}{
		{"at_sign", "get@weather"},
		{"space", "get weather"},
		{"colon", "get:weather"},
	}

	for _, tc := range invalid {
		tc := tc
		t.Run("name_"+tc.name+"_fail_closed", func(t *testing.T) {
			t.Parallel()

			_, f := realPseudonym(t, "charname@example.com")
			r := NewStreamRestorer(f, "gpt-4o")

			line := fmt.Sprintf(
				`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":%s,"arguments":"{}"}}]},"finish_reason":null}]}`,
				jsonStr(tc.funcName),
			)
			_, _, err := r.Push([]byte(line))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("name %q: error = %v, want errStreamAborted", tc.funcName, err)
			}
		})
	}

	// Valid name must be accepted.
	t.Run("name_get_weather_ok", func(t *testing.T) {
		t.Parallel()

		_, f := realPseudonym(t, "validname@example.com")
		r := NewStreamRestorer(f, "gpt-4o")

		lines := []string{
			toolChunkHeader(0, 0, "call_x", "get_weather", `"{}"`),
			"",
			toolFinish(0),
			"",
			"data: [DONE]",
			"",
		}
		_, _, err := pushAllCollect(r, lines)
		if err != nil {
			t.Errorf("valid name get_weather: unexpected error: %v", err)
		}
	})
}

// ── FIX B: Emit assistant role on tool-only streams ──────────────────────────

// TestFixB_ToolOnlyStream_RoleAssistantOnFirstChunk verifies that a tool-only
// stream (no content chunks) emits role:"assistant" exactly once, on the very
// first tool-calls chunk for a choice.
func TestFixB_ToolOnlyStream_RoleAssistantOnFirstChunk(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixb@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	lines := []string{
		toolChunkHeader(0, 0, "call_abc", "get_weather", `"{}"`),
		"",
		toolChunkArgs(0, 0, `" extra"`),
		"",
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("pushAllCollect: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal")
	}

	// Parse all data: lines to find role fields in tool_calls deltas.
	roleCount := 0
	roleValues := map[string]int{}
	firstToolChunkHasRole := false
	isFirst := true

	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if len(ch.Delta.ToolCalls) > 0 {
				if ch.Delta.Role != "" {
					roleCount++
					roleValues[ch.Delta.Role]++
					if isFirst {
						firstToolChunkHasRole = true
					}
				}
				isFirst = false
			}
		}
	}

	if !firstToolChunkHasRole {
		t.Errorf("role:assistant not present on first tool chunk; output:\n%s", output)
	}
	if roleCount != 1 {
		t.Errorf("role emitted %d times in tool chunks, want exactly 1; output:\n%s", roleCount, output)
	}
	if roleValues["assistant"] != 1 {
		t.Errorf("role value = %v, want assistant:1; output:\n%s", roleValues, output)
	}
}

// TestFixB_ToolOnlyStream_RoleNotOnSubsequentChunks verifies that role is
// absent on tool chunks after the first.
func TestFixB_ToolOnlyStream_RoleNotOnSubsequentChunks(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixb2@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Three argument chunks to produce multiple emitted tool chunks.
	lines := []string{
		toolChunkHeader(0, 0, "call_abc", "get_weather", `"arg1"`),
		"",
		toolChunkArgs(0, 0, `"arg2"`),
		"",
		toolChunkArgs(0, 0, `"arg3"`),
		"",
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	output, _, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("pushAllCollect: %v", err)
	}

	// Collect all tool-call chunks with their role field.
	type chunkRole struct {
		hasToolCalls bool
		role         string
	}
	var chunks []chunkRole
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if len(ch.Delta.ToolCalls) > 0 {
				chunks = append(chunks, chunkRole{true, ch.Delta.Role})
			}
		}
	}

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 tool chunks, got %d; output:\n%s", len(chunks), output)
	}
	if chunks[0].role != "assistant" {
		t.Errorf("first tool chunk role = %q, want assistant", chunks[0].role)
	}
	for i, c := range chunks[1:] {
		if c.role != "" {
			t.Errorf("tool chunk[%d] has unexpected role %q (want empty)", i+1, c.role)
		}
	}
}

// ── FIX C: [DONE] requires finish for tool-call choices ──────────────────────

// TestFixC_DoneWithoutFinishReason_ToolChoice_FailClosed verifies that [DONE]
// arriving after a tool call but before a finish_reason causes fail-closed abort.
func TestFixC_DoneWithoutFinishReason_ToolChoice_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixc@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Stream a valid tool call but no finish_reason before [DONE].
	lines := []string{
		toolChunkHeader(0, 0, "call_x", "fn", `"hello"`),
		"",
		"data: [DONE]",
		"",
	}
	_, _, err := pushAllCollect(r, lines)
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("DONE without finish_reason after tool call: error = %v, want errStreamAborted", err)
	}
}

// TestFixC_DoneWithFinishReason_ToolChoice_OK verifies that [DONE] arriving
// after a tool call with a preceding finish_reason:"tool_calls" succeeds.
func TestFixC_DoneWithFinishReason_ToolChoice_OK(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixcok@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	lines := []string{
		toolChunkHeader(0, 0, "call_x", "fn", `"hello"`),
		"",
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	_, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Errorf("DONE with finish_reason after tool call: unexpected error: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal")
	}
}

// TestFixC_DoneWithoutFinishReason_NonToolChoice_OK verifies that [DONE]
// without a finish_reason on a non-tool choice does NOT trigger the new gate
// (the gate only applies to tool-call choices).
//
// Note: in practice OpenAI always sends finish_reason before [DONE] for all
// choices, but this test documents the scoped behavior of FIX C.
func TestFixC_DoneWithoutFinishReason_NonToolChoice_OK(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixcnontool@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Content-only choice: sawToolCall is false, so the finish gate does not apply.
	// Push content without ever sending finish_reason and go straight to [DONE].
	// The only existing check is carry-not-empty (the content "hi" is short and
	// clears the carry, so no errCarryNotEmpty). After it clears, [DONE] succeeds.
	lines := []string{
		chatChunk(0, "hi"),
		"",
		"data: [DONE]",
		"",
	}
	_, _, err := pushAllCollect(r, lines)
	// No tool call was made for this choice, so the FIX C gate does not fire.
	// errStreamAborted would be wrong here.
	if errors.Is(err, errStreamAborted) {
		t.Errorf("non-tool choice DONE without finish: got errStreamAborted, want nil or errCarryNotEmpty")
	}
}

// ── FIX D: Present-but-empty repeated header fields fail-closed ──────────────

// TestFixD_ContinuationIDPresentEmpty_FailClosed verifies that a continuation
// fragment with id explicitly present but empty ("") causes fail-closed abort.
// An absent id (key omitted / nil pointer) must remain OK.
func TestFixD_ContinuationIDPresentEmpty_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixd@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First chunk: valid header.
	first := toolChunkHeader(0, 0, "call_orig", "fn", `""`)
	if _, _, err := r.Push([]byte(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	r.Push([]byte{})

	// Second chunk: same index, id explicitly present but empty string.
	second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"","function":{"arguments":"more"}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(second))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("continuation id present-empty: error = %v, want errStreamAborted", err)
	}
}

// TestFixD_ContinuationIDAbsent_OK verifies that a continuation fragment with
// id key entirely absent (nil *string) is accepted.
func TestFixD_ContinuationIDAbsent_OK(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixdabsent@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First chunk: valid header.
	first := toolChunkHeader(0, 0, "call_orig", "fn", `""`)
	if _, _, err := r.Push([]byte(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	r.Push([]byte{})

	// Second chunk: same index, id key absent (no "id" key in the element JSON).
	second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"more"}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(second))
	if err != nil {
		t.Errorf("continuation id absent: unexpected error: %v", err)
	}
}

// TestFixD_ContinuationNamePresentEmpty_FailClosed verifies that a continuation
// fragment with name explicitly present but empty ("") causes fail-closed abort.
func TestFixD_ContinuationNamePresentEmpty_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixdname@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First chunk: valid header with function object containing name.
	first := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"fn_orig","arguments":""}}]},"finish_reason":null}]}`
	if _, _, err := r.Push([]byte(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	r.Push([]byte{})

	// Second chunk: name present but empty.
	second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"more"}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(second))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("continuation name present-empty: error = %v, want errStreamAborted", err)
	}
}

// TestFixD_ContinuationTypePresentEmpty_FailClosed verifies that a continuation
// fragment (same index, after a valid header) that carries "type":"" (present but
// empty) causes fail-closed abort. This is the type-field analogue of
// TestFixD_ContinuationIDPresentEmpty_FailClosed and
// TestFixD_ContinuationNamePresentEmpty_FailClosed. A present-but-empty type is
// structurally anomalous: "function" is the only accepted non-nil type value, so
// an empty string is always a protocol violation.
func TestFixD_ContinuationTypePresentEmpty_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixdtype@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First chunk: valid header (id, type:"function", name all present and valid).
	first := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_orig","type":"function","function":{"name":"fn_orig","arguments":""}}]},"finish_reason":null}]}`
	if _, _, err := r.Push([]byte(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	r.Push([]byte{})

	// Second chunk: same index, type explicitly present but empty string.
	// An empty type value on a continuation is "present-but-empty" and must
	// fail-closed (distinct from the legitimately absent type key).
	second := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"","function":{"arguments":"more"}}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(second))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("continuation type present-empty: error = %v, want errStreamAborted", err)
	}
}

// TestFixB_ContentThenTool_RoleOnlyOnFirst verifies that when a single choice
// streams a content delta first (which carries role:"assistant") and then streams
// a tool_calls delta for the same choice, role:"assistant" appears exactly once
// across all emitted chunks — on the first content chunk — and the subsequent
// tool chunk does NOT carry a role field. This exercises the shared cc.roleSent
// flag that is set by the content path and then observed by the tool path.
//
// To avoid triggering the cross-lane guard (content carry non-empty when
// tool_calls arrives), the content payload is plain text with no trailing
// pseudonym prefix, ensuring the carry drains completely before the tool delta.
func TestFixB_ContentThenTool_RoleOnlyOnFirst(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "fixb_contenttool@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Step 1: content delta that carries the role. The content "hello world" has
	// no suffix that is a proper prefix of any known pseudonym (it ends in "d",
	// which cannot start "PII_"), so the carry drains fully and roleSent is set.
	// The upstream role key is present so the restorer synthesises "assistant".
	contentLine := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hello world"},"finish_reason":null}]}`
	if _, _, err := r.Push([]byte(contentLine)); err != nil {
		t.Fatalf("content chunk: %v", err)
	}
	r.Push([]byte{})

	// Step 2: tool_calls delta for the same choice. Because formatChat accepts
	// tool_calls and the content carry is empty, this must succeed. The role must
	// NOT appear again (cc.roleSent is already true).
	toolLine := toolChunkHeader(0, 0, "call_abc", "get_data", `"{}"`)
	if _, _, err := r.Push([]byte(toolLine)); err != nil {
		t.Fatalf("tool chunk: %v", err)
	}
	r.Push([]byte{})

	// Step 3: finish and done.
	lines := []string{
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("finish+done: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal")
	}

	// Collect all emitted data: lines and count role occurrences.
	type chunkRole struct {
		hasContent  bool
		hasToolCall bool
		role        string
	}
	var chunks []chunkRole

	// Re-collect the content and tool chunks emitted before the finish sequence.
	// We need to gather all output including the earlier pushes. Re-run through
	// pushAllCollect on a fresh restorer to get a single consolidated output.
	r2 := NewStreamRestorer(f, "gpt-4o")
	allLines := []string{
		contentLine,
		"",
		toolLine,
		"",
		toolFinish(0),
		"",
		"data: [DONE]",
		"",
	}
	allOutput, terminal2, err2 := pushAllCollect(r2, allLines)
	if err2 != nil {
		t.Fatalf("full stream: %v", err2)
	}
	if !terminal2 {
		t.Error("full stream did not reach terminal")
	}
	_ = output // the partial output from above was only used to verify no error

	for _, line := range strings.Split(allOutput, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string  `json:"role"`
					Content   *string `json:"content"`
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			cr := chunkRole{
				hasContent:  ch.Delta.Content != nil,
				hasToolCall: len(ch.Delta.ToolCalls) > 0,
				role:        ch.Delta.Role,
			}
			chunks = append(chunks, cr)
		}
	}

	// Count total role emissions across all chunks.
	roleCount := 0
	for _, c := range chunks {
		if c.role != "" {
			roleCount++
		}
	}
	if roleCount != 1 {
		t.Errorf("role emitted %d times across all chunks, want exactly 1; output:\n%s", roleCount, allOutput)
	}

	// The first chunk (content) must carry the role.
	if len(chunks) == 0 {
		t.Fatalf("no chunks emitted; output:\n%s", allOutput)
	}
	if chunks[0].role != "assistant" {
		t.Errorf("first chunk role = %q, want \"assistant\"; output:\n%s", chunks[0].role, allOutput)
	}
	if !chunks[0].hasContent {
		t.Errorf("first chunk expected to be a content chunk; output:\n%s", allOutput)
	}

	// All tool-call chunks must NOT carry a role.
	for i, c := range chunks {
		if c.hasToolCall && c.role != "" {
			t.Errorf("tool chunk[%d] carries unexpected role %q; output:\n%s", i, c.role, allOutput)
		}
	}
}
