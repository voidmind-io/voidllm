package pii

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// dataLinePrefix is the SSE field prefix per RFC 6202 §4.2. A "data" field
// line MUST begin with "data:" — the space after the colon is optional.
var dataLinePrefix = []byte("data:")

// donePayload is the SSE stream termination sentinel.
var donePayload = []byte("[DONE]")

// errStreamTerminated is returned by StreamRestorer.Push when input is
// received after the stream has already emitted its terminal [DONE] line.
var errStreamTerminated = errors.New("pii: stream already terminated")

// errStreamAborted is returned when the stream encounters a fatal protocol
// violation and the restorer enters a permanent fail-closed state.
var errStreamAborted = errors.New("pii: stream aborted due to protocol violation")

// errCarryNotEmpty is returned when [DONE] arrives but one or more choice
// carries are non-empty (truncated pseudonym — the upstream never completed
// the token it started).
var errCarryNotEmpty = errors.New("pii: stream ended with incomplete pseudonym in carry buffer")

// maxPIIStreamBytes is the aggregate byte cap for carry buffers across all
// choices in a single streaming request. A carry can only hold up to
// pseudonymLen-1 bytes per choice, so in practice this cap is never reached
// for well-behaved upstreams; it guards against pathological inputs.
const maxPIIStreamBytes = 50 * 1024 * 1024 // 50 MB

// maxStreamChoices is the maximum number of distinct choice indices allowed
// across all chunks in a single streaming request. Upstreams that emit
// millions of distinct indices would otherwise grow s.choices without bound.
// 128 covers every legitimate LLM use-case with generous headroom.
const maxStreamChoices = 128

// allowedFinishReasons is the set of finish_reason values that the restorer
// accepts from the upstream. Any value not in this set is a protocol violation
// and causes fail-closed abort. An empty or null finish_reason is never
// passed to this check (the caller guards with *rc.FinishReason != "").
//
// "tool_calls" and "function_call" are intentionally excluded: the Anthropic
// adapter translates tool_use stop_reason to "tool_calls" but drops all
// tool_use content deltas (they are not text and cannot be PII-restored).
// This means the StreamRestorer would never see any tool_calls delta (so the
// delta-level fail-closed check never fires), yet the stream closes with
// finish_reason:"tool_calls" — producing a response with no tool_calls body
// but the wrong finish_reason. Excluding these values from the allowlist
// ensures that any stream involving tool use is fail-closed, regardless of
// whether the tool_calls delta was visible to the restorer.
var allowedFinishReasons = map[string]bool{
	"stop":           true,
	"length":         true,
	"content_filter": true,
}

// choiceFormat is the detected format of a streaming choice's content field.
type choiceFormat int

const (
	// formatUnknown means no content delta has been observed yet.
	formatUnknown choiceFormat = iota
	// formatChat means content arrives in choices[i].delta.content.
	formatChat
	// formatCompletion means content arrives in choices[i].text.
	formatCompletion
	// formatRefusal means content arrives in choices[i].delta.refusal.
	// OpenAI gpt-4o and later models stream model-generated refusal text in this
	// field when the model declines to answer. It is treated identically to
	// delta.content for PII restore purposes (the text may contain pseudonyms),
	// but is re-emitted as delta.refusal so the client sees the correct field.
	formatRefusal
)

// choiceCarry holds the per-choice rolling-buffer state for StreamRestorer.
type choiceCarry struct {
	// carry is the accumulated, not-yet-emitted content bytes. Invariant: carry
	// is either empty or a proper prefix of a known pseudonym — never arbitrary
	// buffered text, never an emittable fragment.
	carry []byte
	// format is set on the first delta with content and never changes.
	format choiceFormat
	// role is emitted once with the first content chunk.
	role string
	// roleSent tracks whether the role has already been emitted.
	roleSent bool
	// finishReason is held until carry is empty (which it must be by then,
	// else fail-closed) and emitted as a separate finish chunk.
	finishReason string
	// finished is true after a finish_reason has been seen; content after this
	// point is a protocol violation (fail-closed).
	finished bool
}

// StreamRestorer performs incremental, content-aware PII restore over a
// line-by-line upstream SSE stream. Unlike the buffered RestoreSSEStream
// (preserved as a test oracle in stream_oracle_test.go), StreamRestorer
// delivers restored content to the client token-by-token as soon as it is
// safe to do so — that is, as soon as the carry buffer cannot be the start of
// a known pseudonym.
//
// Pseudonyms are exactly pseudonymLen (31) bytes and begin with pseudonymMarker
// ("PII_"). The restorer maintains a per-choice carry buffer of at most
// pseudonymLen-1 bytes. A byte is emitted only when the longest suffix of the
// carry that is a proper prefix of ANY known pseudonym has length L, making the
// first len(carry)-L bytes safe to emit.
//
// Availability trade-off: if the upstream response ends (via [DONE]) while any
// per-choice carry is non-empty, the restorer returns errCarryNotEmpty and
// aborts the stream fail-closed. This covers the case where natural content
// happens to end on a proper prefix of a known pseudonym (e.g. the response
// legitimately ends with a capital "P", "PI", "PII", or "PII_"). Because
// truncation is indistinguishable from a partial pseudonym at the stream
// boundary, the restorer cannot safely emit the held bytes. This is a
// deliberate, leak-safe design choice: the alternative — emitting ambiguous
// trailing bytes — risks exposing a real pseudonym fragment to the client.
//
// Concurrency: StreamRestorer is NOT safe for concurrent use. It is designed
// for single-goroutine use inside the streaming SendStreamWriter closure.
//
// Usage:
//
//	restorer := pii.NewStreamRestorer(filter, "gpt-4o")
//	for scanner.Scan() {
//	    out, terminal, err := restorer.Push(scanner.Bytes())
//	    // handle out, terminal, err
//	}
type StreamRestorer struct {
	filter    *Filter
	modelName string
	// chunkID is a proxy-generated chat completion ID, fixed for all chunks
	// in this response so clients can correlate them.
	chunkID string
	// created is the proxy-generated Unix timestamp for all synthesized chunks.
	created int64
	// choices maps choice index to per-choice carry state.
	choices map[int]*choiceCarry
	// inEvent is true when we are inside an SSE event (after "data:" but before blank).
	inEvent bool
	// terminal is true after [DONE] has been processed.
	terminal bool
	// aborted is true after an unrecoverable error; all further Pushes return errStreamAborted.
	aborted bool
	// sortedPseudonyms is a sorted snapshot of all known pseudonyms, taken at
	// construction time. It is used by longestPseudonymPrefixSuffix to determine
	// via binary search whether any suffix of the carry buffer is a proper prefix
	// of a known pseudonym. Sorting is O(N log N) once at construction; each
	// per-suffix lookup is O(log N + M) where M is the length of the suffix
	// candidate (bounded by pseudonymLen-1 = 30). Memory is O(N) — a sorted
	// copy of the pseudonym strings, comparable in size to what the Filter holds.
	sortedPseudonyms []string
	// totalCarryBytes is the sum of carry lengths across all choices.
	// Used to enforce maxPIIStreamBytes.
	totalCarryBytes int
}

// NewStreamRestorer creates a StreamRestorer for a single proxy request.
// filter must be the per-request Filter whose rev map contains all pseudonyms
// that were injected into the upstream request. modelName is the canonical
// model name to embed in synthesized SSE envelopes.
//
// NewStreamRestorer snapshots the known pseudonyms from filter at construction
// time (via filter.pseudonyms()). Pseudonyms added to filter after construction
// are not considered; for the streaming response path this is correct because
// AnonymizeJSON has already completed before the upstream response begins.
//
// The sorted pseudonym slice for longestPseudonymPrefixSuffix is built here
// once in O(N log N). Per-suffix binary lookups are then O(log N) versus the
// former O(N * pseudonymLen) map-construction cost that materialized every
// proper prefix. Memory is O(N) rather than O(N * pseudonymLen).
func NewStreamRestorer(filter *Filter, modelName string) *StreamRestorer {
	v7, err := uuid.NewV7()
	if err != nil {
		v7 = uuid.New()
	}
	knownPseudonyms := filter.pseudonyms()
	sorted := makeSortedPseudonyms(knownPseudonyms)
	return &StreamRestorer{
		filter:           filter,
		modelName:        modelName,
		chunkID:          "chatcmpl-" + v7.String(),
		created:          time.Now().Unix(),
		choices:          make(map[int]*choiceCarry),
		sortedPseudonyms: sorted,
	}
}

// makeSortedPseudonyms returns a lexicographically sorted copy of the given
// pseudonym slice. The sorted order enables binary search in
// longestPseudonymPrefixSuffix: to test whether any pseudonym has a given
// string as a (proper) prefix, find the first pseudonym >= the candidate
// string and check whether it starts with that string.
func makeSortedPseudonyms(pseudonyms []string) []string {
	if len(pseudonyms) == 0 {
		return nil
	}
	sorted := make([]string, len(pseudonyms))
	copy(sorted, pseudonyms)
	sort.Strings(sorted)
	return sorted
}

// Push consumes one raw upstream SSE line (as returned by bufio.Scanner.Bytes,
// without trailing newline) and returns zero or more ready-to-emit SSE lines.
//
// A nil element in the returned slice represents a blank SSE event separator
// (write a bare '\n' to the wire). Non-nil elements are complete SSE lines
// without trailing newline (write the bytes then a '\n').
//
// terminal is true when the [DONE] sentinel has been processed. The caller
// must break its scan loop when terminal is true and MUST NOT call Push again.
//
// Fail-closed contract:
//   - Any protocol violation (tool_calls, double data: in one event, content
//     after finish_reason, upstream error object, etc.) sets the restorer to
//     aborted state and returns errStreamAborted. On error the caller must stop
//     emitting; no further content is safe.
//   - [DONE] with a non-empty carry on any choice → errCarryNotEmpty.
//   - Any Push after terminal or aborted → errStreamTerminated / errStreamAborted.
func (s *StreamRestorer) Push(line []byte) (out [][]byte, terminal bool, err error) {
	if s.aborted {
		return nil, false, errStreamAborted
	}
	if s.terminal {
		return nil, false, errStreamTerminated
	}

	// Blank line: SSE event separator. Reset inEvent.
	if len(line) == 0 {
		s.inEvent = false
		return nil, false, nil
	}

	// Non-data lines (SSE comment, event:, id:, retry:) are discarded.
	// They MUST NOT be forwarded: upstream envelope fields (id, model,
	// system_fingerprint) may echo pseudonyms. By synthesizing our own
	// envelope we eliminate that channel entirely.
	if !bytes.HasPrefix(line, dataLinePrefix) {
		return nil, false, nil
	}

	// Strip "data:" and optional single space BEFORE the multi-line check so
	// that [DONE] detection can happen independently of SSE event state.
	payload := line[len(dataLinePrefix):]
	if len(payload) > 0 && payload[0] == ' ' {
		payload = payload[1:]
	}

	// [DONE] sentinel must be checked BEFORE the inEvent/multi-line guard.
	// Some adapters (e.g. Gemini) emit the blank SSE separator AFTER the last
	// content chunk but then emit "data: [DONE]" without a preceding blank,
	// which means the restorer still has inEvent=true from the previous data
	// line when it encounters [DONE]. Treating [DONE] as a multi-line violation
	// would incorrectly abort every Gemini stream that passes through the PII
	// restorer. [DONE] is the SSE stream terminator, not a content line, and
	// is always safe to process regardless of inEvent state.
	if bytes.Equal(payload, donePayload) {
		return s.handleDone()
	}

	// Detect multi-line data: within a single SSE event (before blank separator).
	// Two genuine JSON content data: lines without an intervening blank separator
	// is a protocol violation — fail-closed.
	if s.inEvent {
		s.aborted = true
		return nil, false, errStreamAborted
	}
	s.inEvent = true

	// Must be JSON.
	var rawDoc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(payload, &rawDoc); err != nil {
		s.aborted = true
		return nil, false, errStreamAborted
	}

	// Fail-closed: upstream-level error object.
	if _, hasError := rawDoc["error"]; hasError {
		s.aborted = true
		return nil, false, errStreamAborted
	}

	// Usage-only chunk: choices is present and is a strictly empty JSON array.
	rawChoicesJSON, hasChoices := rawDoc["choices"]
	if hasChoices {
		// Check for strictly empty array first.
		var choicesArr []jsonx.RawMessage
		if err2 := jsonx.Unmarshal(rawChoicesJSON, &choicesArr); err2 != nil {
			s.aborted = true
			return nil, false, errStreamAborted
		}
		if len(choicesArr) == 0 {
			// Usage-only chunk: re-emit a whitelisted usage chunk.
			rawUsage, hasUsage := rawDoc["usage"]
			if !hasUsage {
				// Empty choices with no usage: benign, skip.
				return nil, false, nil
			}
			usageLine, uerr := s.buildUsageChunk(rawUsage)
			if uerr != nil {
				// Cannot parse usage safely; skip (not fail-closed — usage is metadata).
				return nil, false, nil
			}
			return [][]byte{usageLine, nil}, false, nil
		}
		// Non-empty choices: process normally below.
		return s.handleChoices(choicesArr)
	}

	// Chunk with no "choices" field at all: skip (auxiliary data).
	return nil, false, nil
}

// handleDone processes the [DONE] sentinel.
func (s *StreamRestorer) handleDone() ([][]byte, bool, error) {
	// Verify all carries are empty. A non-empty carry means the upstream
	// truncated a pseudonym — fail-closed.
	for _, cc := range s.choices {
		if len(cc.carry) > 0 {
			s.aborted = true
			return nil, false, errCarryNotEmpty
		}
	}

	// Emit any pending finish_reason chunks (should already be emitted inline,
	// but guard defensively).
	var out [][]byte
	for _, idx := range sortedChoiceKeys(s.choices) {
		cc := s.choices[idx]
		if cc.finishReason != "" {
			line, err := s.buildFinishChunk(idx, cc.role, cc.finishReason)
			if err != nil {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			out = append(out, line, nil)
			cc.finishReason = ""
		}
	}

	// Emit [DONE].
	out = append(out, []byte("data: [DONE]"), nil)
	s.terminal = true
	return out, true, nil
}

// handleChoices processes a non-empty choices array from one upstream SSE chunk.
func (s *StreamRestorer) handleChoices(rawChoices []jsonx.RawMessage) ([][]byte, bool, error) {
	// Parse choices as typed structs.
	type rawDelta struct {
		Role    *string `json:"role"`
		Content *string `json:"content"`
		Refusal *string `json:"refusal"`
		// ToolCalls is parsed via the raw map for key-presence detection (see below).
	}
	type rawChoice struct {
		Index        *int              `json:"index"`
		Delta        *jsonx.RawMessage `json:"delta"`
		Text         *string           `json:"text"`
		FinishReason *string           `json:"finish_reason"`
	}

	var out [][]byte

	// seenIndices tracks which choice indices have appeared in this chunk so
	// that duplicate indices within a single choices array are detected.
	seenIndices := make(map[int]struct{}, len(rawChoices))

	for _, rawC := range rawChoices {
		var rc rawChoice
		if err := jsonx.Unmarshal(rawC, &rc); err != nil {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// Missing or null index is a protocol violation.
		if rc.Index == nil {
			s.aborted = true
			return nil, false, errStreamAborted
		}
		choiceIdx := *rc.Index

		// Negative index is a protocol violation.
		if choiceIdx < 0 {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// Duplicate index within this chunk is a protocol violation.
		if _, dup := seenIndices[choiceIdx]; dup {
			s.aborted = true
			return nil, false, errStreamAborted
		}
		seenIndices[choiceIdx] = struct{}{}

		// Track which content-bearing field is active in this chunk.
		hasDeltaContent := false
		hasDeltaRefusal := false
		hasTextContent := false
		var contentStr string
		// deltaHasRole is true when the upstream delta contained a "role" key
		// (regardless of value). Used after cc is obtained to synthesize the
		// canonical "assistant" role without ever storing the upstream value.
		deltaHasRole := false

		if rc.Delta != nil {
			// Unmarshal the delta into both a typed struct (for content/refusal)
			// and a raw key map (for key-presence checks on tool_calls, function_call,
			// and role). A single unmarshal pass produces both.
			var delta rawDelta
			if err := jsonx.Unmarshal(*rc.Delta, &delta); err != nil {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			var rawDeltaMap map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(*rc.Delta, &rawDeltaMap); err != nil {
				s.aborted = true
				return nil, false, errStreamAborted
			}

			// Audit every delta key for unknown content-bearing fields. Known
			// content fields are content, refusal, and the legacy text field on
			// the choice level. tool_calls and function_call are always
			// fail-closed, regardless of whether they are null — key presence
			// alone signals tool-call intent and that path is not safe to
			// forward through the PII restorer. A null tool_calls or
			// function_call unmarshals to nil in the typed struct and is
			// indistinguishable from "field absent", so we check the raw map.
			for k := range rawDeltaMap {
				switch k {
				case "role", "content", "refusal":
					// Explicitly handled fields.
				case "tool_calls":
					// Fail-closed: tool_calls key present (even if null/empty).
					s.aborted = true
					return nil, false, errStreamAborted
				case "function_call":
					// function_call in delta: fail-closed regardless of value.
					s.aborted = true
					return nil, false, errStreamAborted
				}
				// All other unrecognised delta fields are conservatively ignored.
				// They are not forwarded (the envelope is synthesized), so unknown
				// fields cannot leak pseudonyms through unhandled paths.
			}

			if delta.Content != nil {
				hasDeltaContent = true
				contentStr = *delta.Content
			}
			if delta.Refusal != nil {
				hasDeltaRefusal = true
				contentStr = *delta.Refusal
			}

			_, deltaHasRole = rawDeltaMap["role"]
		}

		if rc.Text != nil {
			hasTextContent = true
			contentStr = *rc.Text
		}

		// A single chunk must not mix content-bearing fields. Mixing any two of
		// delta.content, delta.refusal, and choice-level text is a protocol
		// violation: fail-closed.
		activeFields := 0
		if hasDeltaContent {
			activeFields++
		}
		if hasDeltaRefusal {
			activeFields++
		}
		if hasTextContent {
			activeFields++
		}
		if activeFields > 1 {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// Get or create per-choice carry. Enforce the active-choice cap BEFORE
		// inserting so that a new index at exactly maxStreamChoices is allowed
		// (0-indexed: indices 0..maxStreamChoices-1 are valid).
		cc, exists := s.choices[choiceIdx]
		if !exists {
			if len(s.choices) >= maxStreamChoices {
				// Memory-DoS guard: too many distinct choice indices.
				s.aborted = true
				return nil, false, errStreamAborted
			}
			cc = &choiceCarry{format: formatUnknown}
			s.choices[choiceIdx] = cc
		}

		// Role synthesis: if the upstream delta contained a "role" key, synthesize
		// the canonical "assistant" role for emission on the first content chunk.
		// We NEVER echo the upstream role value — a malicious upstream could embed
		// a pseudonym or arbitrary string there. The only valid assistant-response
		// role is "assistant"; the restorer unconditionally synthesizes this value.
		// For legacy-completion format (choices[].text), role is never emitted.
		if deltaHasRole && !cc.roleSent && cc.role == "" {
			cc.role = "assistant"
		}

		// Fail-closed: content after finish_reason.
		if cc.finished && activeFields > 0 {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// Detect and lock in format. A choice that changes its content field
		// mid-stream (e.g., content then refusal) is a protocol violation.
		if hasDeltaContent {
			switch cc.format {
			case formatUnknown:
				cc.format = formatChat
			case formatChat:
				// Consistent — continue.
			default:
				s.aborted = true
				return nil, false, errStreamAborted
			}
		}
		if hasDeltaRefusal {
			switch cc.format {
			case formatUnknown:
				cc.format = formatRefusal
			case formatRefusal:
				// Consistent — continue.
			default:
				s.aborted = true
				return nil, false, errStreamAborted
			}
		}
		if hasTextContent {
			switch cc.format {
			case formatUnknown:
				cc.format = formatCompletion
			case formatCompletion:
				// Consistent — continue.
			default:
				s.aborted = true
				return nil, false, errStreamAborted
			}
		}

		// Process content through the rolling buffer.
		if activeFields > 0 {
			emitLines, err := s.pushContent(choiceIdx, cc, contentStr)
			if err != nil {
				s.aborted = true
				return nil, false, err
			}
			out = append(out, emitLines...)
		}

		// Process finish_reason.
		if rc.FinishReason != nil && *rc.FinishReason != "" {
			reason := *rc.FinishReason
			// Validate against the allowlist. A finish_reason outside the
			// allowlist (including a pseudonym or arbitrary upstream string)
			// is a protocol violation — fail-closed.
			if !allowedFinishReasons[reason] {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			if cc.finished {
				// Duplicate finish_reason: fail-closed.
				s.aborted = true
				return nil, false, errStreamAborted
			}
			cc.finished = true
			// Carry must be empty here by the rolling-buffer invariant. If it
			// is not empty, the upstream truncated a pseudonym.
			if len(cc.carry) > 0 {
				s.aborted = true
				return nil, false, errCarryNotEmpty
			}
			// Emit the finish chunk immediately.
			line, err := s.buildFinishChunk(choiceIdx, cc.role, reason)
			if err != nil {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			out = append(out, line, nil)
			cc.finishReason = "" // already emitted
		}
	}

	return out, false, nil
}

// pushContent appends content to the per-choice carry and emits safe bytes.
// Returns the SSE lines to emit (may be empty).
func (s *StreamRestorer) pushContent(choiceIdx int, cc *choiceCarry, content string) ([][]byte, error) {
	// Append to carry.
	before := len(cc.carry)
	cc.carry = append(cc.carry, content...)
	s.totalCarryBytes += len(cc.carry) - before

	if s.totalCarryBytes > maxPIIStreamBytes {
		return nil, errStreamAborted
	}

	// Determine how many bytes of carry are safe to emit.
	// L = length of the longest suffix of carry that is a proper prefix of a
	// known pseudonym. B = len(carry) - L bytes can be emitted.
	L := s.longestPseudonymPrefixSuffix(cc.carry)
	B := len(cc.carry) - L
	if B <= 0 {
		// Nothing safe to emit yet.
		return nil, nil
	}

	safe := cc.carry[:B]
	// Apply Restore to the safe portion.
	restored := s.filter.Restore(safe)

	// Advance carry.
	carry := cc.carry[B:]
	cc.carry = make([]byte, len(carry))
	copy(cc.carry, carry)
	s.totalCarryBytes -= B

	// Build SSE output lines.
	contentChunk := string(restored)
	line, err := s.buildContentChunk(choiceIdx, cc, contentChunk)
	if err != nil {
		return nil, errStreamAborted
	}
	// Mark role as sent after first content emission.
	cc.roleSent = true

	return [][]byte{line, nil}, nil
}

// longestPseudonymPrefixSuffix returns L: the length of the longest suffix of
// carry that is a proper prefix (i.e., shorter than pseudonymLen) of ANY known
// pseudonym. Returns 0 if no suffix matches.
//
// Since all pseudonyms share the prefix "PII_", any carry suffix that does not
// begin with some prefix of "PII_" cannot be a prefix of any pseudonym.
// couldBePseudonymPrefix is checked first as a cheap structural early-exit.
//
// Performance: sortedPseudonyms (built once in O(N log N) in NewStreamRestorer)
// allows O(log N) binary search per suffix candidate via sort.Search. The
// inner loop runs at most pseudonymLen-1 = 30 iterations, each doing an
// O(log N) binary search and an O(M) HasPrefix check (M <= 30). Total cost
// per pushContent call is O(pseudonymLen * log N) — independent of the number
// of distinct pseudonyms. This replaces the former O(N * pseudonymLen) map
// construction that allocated up to ~300 000 map entries per request.
func (s *StreamRestorer) longestPseudonymPrefixSuffix(carry []byte) int {
	if len(s.sortedPseudonyms) == 0 {
		return 0
	}
	n := len(carry)
	// Check suffixes from longest to shortest (skip length 0 and length=pseudonymLen).
	// A suffix of length pseudonymLen would be a complete pseudonym — it would be
	// replaced by Restore rather than held; we only hold proper prefixes.
	maxCheck := n
	if maxCheck >= pseudonymLen {
		maxCheck = pseudonymLen - 1
	}
	for l := maxCheck; l >= 1; l-- {
		suffix := string(carry[n-l:])
		// Quick structural filter: a valid pseudonym prefix must match the
		// leading structure of "PII_". Skip suffixes that cannot possibly
		// start any known pseudonym.
		if !couldBePseudonymPrefix(suffix) {
			continue
		}
		// Binary search: find the first pseudonym >= suffix.
		// If that pseudonym starts with suffix, then suffix is a proper prefix
		// of a known pseudonym (since len(suffix) < pseudonymLen by construction).
		idx := sort.SearchStrings(s.sortedPseudonyms, suffix)
		if idx < len(s.sortedPseudonyms) && strings.HasPrefix(s.sortedPseudonyms[idx], suffix) {
			return l
		}
	}
	return 0
}

// couldBePseudonymPrefix reports whether s could be a proper prefix of a
// pseudonym. All pseudonyms begin with "PII_", so a candidate string of
// length l can only be a prefix if it matches the first l characters of
// "PII_<..." — it must be a prefix of pseudonymMarker, or pseudonymMarker
// must be a prefix of it (i.e., it starts with "PII_").
func couldBePseudonymPrefix(s string) bool {
	marker := pseudonymMarker
	if len(s) <= len(marker) {
		// s could be the beginning of "PII_": check that marker starts with s.
		return strings.HasPrefix(marker, s)
	}
	// s is longer than "PII_": it must start with "PII_" to be a pseudonym prefix.
	return strings.HasPrefix(s, marker)
}

// ── Typed structs for synthesized SSE JSON ────────────────────────────────────

// sseEnvelope is the fixed top-level envelope of every synthesized chunk.
// We never forward the upstream id/model/system_fingerprint because those
// fields may echo pseudonyms.
type sseEnvelope struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	// Choices holds one of the following concrete slice types depending on the
	// chunk kind:
	//   []sseContentChoiceChat          — delta.content chunk (formatChat)
	//   []sseRefusalChoiceChat          — delta.refusal chunk (formatRefusal)
	//   []sseFinishChoiceChat           — finish_reason chunk (chat/refusal)
	//   []sseContentChoiceCompletion    — text chunk (formatCompletion)
	//   []sseFinishChoiceCompletion     — finish_reason chunk (completion)
	//   []interface{}                   — empty array for usage-only chunks
	Choices interface{} `json:"choices"`
}

// sseDeltaChat is the typed delta for chat completion content chunks.
type sseDeltaChat struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content"`
}

// sseDeltaRefusal is the typed delta for chat completion refusal chunks.
// OpenAI gpt-4o and later models stream model-generated refusal text in the
// delta.refusal field when the model declines to answer.
type sseDeltaRefusal struct {
	Role    string  `json:"role,omitempty"`
	Refusal *string `json:"refusal"`
}

// sseDeltaEmpty is the typed delta for finish_reason chunks (no content).
type sseDeltaEmpty struct{}

// sseContentChoiceChat is a typed chat-completion choice with delta.content.
type sseContentChoiceChat struct {
	Index        int          `json:"index"`
	Delta        sseDeltaChat `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

// sseRefusalChoiceChat is a typed chat-completion choice with delta.refusal.
type sseRefusalChoiceChat struct {
	Index        int             `json:"index"`
	Delta        sseDeltaRefusal `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

// sseFinishChoiceChat is a typed chat-completion choice with finish_reason and empty delta.
type sseFinishChoiceChat struct {
	Index        int           `json:"index"`
	Delta        sseDeltaEmpty `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

// sseContentChoiceCompletion is a typed legacy-completion choice with text.
type sseContentChoiceCompletion struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// sseFinishChoiceCompletion is a typed legacy-completion choice with finish_reason.
type sseFinishChoiceCompletion struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// whitelistedUsage contains only the fields we re-emit from upstream usage chunks.
type whitelistedUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ── Chunk builders ────────────────────────────────────────────────────────────

// buildContentChunk builds a data: SSE line with a content delta for the given
// choice. The envelope (id, object, created, model) is synthesized locally —
// never copied from the upstream response — to prevent pseudonym echo.
// The delta field used matches the detected format: delta.content for chat,
// delta.refusal for refusal, and choices[].text for legacy completions.
func (s *StreamRestorer) buildContentChunk(choiceIdx int, cc *choiceCarry, content string) ([]byte, error) {
	// Determine object type and choice shape from format.
	var choices interface{}
	var objectType string

	role := ""
	if !cc.roleSent && cc.role != "" {
		role = cc.role
	}

	switch cc.format {
	case formatChat, formatUnknown:
		objectType = "chat.completion.chunk"
		contentPtr := content
		choices = []sseContentChoiceChat{{
			Index: choiceIdx,
			Delta: sseDeltaChat{Role: role, Content: &contentPtr},
		}}
	case formatRefusal:
		objectType = "chat.completion.chunk"
		refusalPtr := content
		choices = []sseRefusalChoiceChat{{
			Index: choiceIdx,
			Delta: sseDeltaRefusal{Role: role, Refusal: &refusalPtr},
		}}
	case formatCompletion:
		objectType = "text_completion"
		choices = []sseContentChoiceCompletion{{
			Index: choiceIdx,
			Text:  content,
		}}
	}

	env := sseEnvelope{
		ID:      s.chunkID,
		Object:  objectType,
		Created: s.created,
		Model:   s.modelName,
		Choices: choices,
	}

	payload, err := jsonx.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), payload...), nil
}

// buildFinishChunk builds a data: SSE line with a finish_reason for the given
// choice. The envelope is synthesized locally.
func (s *StreamRestorer) buildFinishChunk(choiceIdx int, role string, finishReason string) ([]byte, error) {
	var choices interface{}
	var objectType string

	cc := s.choices[choiceIdx]
	format := formatChat
	if cc != nil {
		format = cc.format
	}

	switch format {
	case formatChat, formatRefusal, formatUnknown:
		// Both chat and refusal choices use the same finish_reason envelope:
		// an empty delta ({}) with the finish_reason field set. The distinction
		// between content and refusal only matters for content chunks.
		objectType = "chat.completion.chunk"
		choices = []sseFinishChoiceChat{{
			Index:        choiceIdx,
			Delta:        sseDeltaEmpty{},
			FinishReason: finishReason,
		}}
	case formatCompletion:
		objectType = "text_completion"
		choices = []sseFinishChoiceCompletion{{
			Index:        choiceIdx,
			Text:         "",
			FinishReason: finishReason,
		}}
	}

	env := sseEnvelope{
		ID:      s.chunkID,
		Object:  objectType,
		Created: s.created,
		Model:   s.modelName,
		Choices: choices,
	}

	payload, err := jsonx.Marshal(env)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), payload...), nil
}

// buildUsageChunk builds a data: SSE line re-emitting only whitelisted usage
// fields. It discards all other fields from the upstream usage object.
func (s *StreamRestorer) buildUsageChunk(rawUsage jsonx.RawMessage) ([]byte, error) {
	var u whitelistedUsage
	if err := jsonx.Unmarshal(rawUsage, &u); err != nil {
		return nil, err
	}

	type usageChunk struct {
		ID      string           `json:"id"`
		Object  string           `json:"object"`
		Created int64            `json:"created"`
		Model   string           `json:"model"`
		Choices []interface{}    `json:"choices"`
		Usage   whitelistedUsage `json:"usage"`
	}
	chunk := usageChunk{
		ID:      s.chunkID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.modelName,
		Choices: []interface{}{},
		Usage:   u,
	}

	payload, err := jsonx.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return append([]byte("data: "), payload...), nil
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// sortedChoiceKeys returns the keys of m sorted in ascending order.
// Used to produce deterministic choice ordering in output SSE.
// Insertion sort is appropriate here: choice counts are always small (1-8).
func sortedChoiceKeys(m map[int]*choiceCarry) []int {
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
