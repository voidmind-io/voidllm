package pii

import (
	"bytes"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// toolIDCharsetRe and toolNameCharsetRe are precompiled conservative charset
// validators for tool-call id and function name respectively. Both use the
// same charset — [A-Za-z0-9_.-]+ — which covers all known provider tool-call
// id formats (e.g. "call_abc123") and valid OpenAI function names. Any
// character outside this set (including '@', spaces, ':', non-ASCII) would
// indicate an encoding anomaly and could carry PII. Such values fail-closed.
var toolIDCharsetRe = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
var toolNameCharsetRe = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

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
// "tool_calls" is included here but is additionally gated at the choice level:
// it is only accepted when cc.sawToolCall is true for that choice (i.e. at
// least one fully-validated, non-empty tool-call element was processed). This
// keeps the Anthropic adapter correctly fail-closed: the Anthropic adapter
// translates tool_use stop_reason to "tool_calls" but drops all tool_use
// content deltas, so sawToolCall is never set and the finish_reason is
// rejected. For OpenAI/vLLM passthrough streams with real tool_calls deltas,
// sawToolCall is set and the finish_reason is accepted.
//
// "function_call" is intentionally excluded: legacy singular delta.function_call
// is always fail-closed and must not be authorized via the finish_reason allowlist.
var allowedFinishReasons = map[string]bool{
	"stop":           true,
	"length":         true,
	"content_filter": true,
	"tool_calls":     true,
}

// maxToolCallsPerChoice is the maximum number of distinct tool-call indices
// allowed per choice. A tool_calls array with more distinct indices than this
// limit is a protocol violation and causes fail-closed abort. 64 covers all
// realistic LLM use cases with generous headroom.
const maxToolCallsPerChoice = 64

// maxToolCallsTotal is the global cap on distinct tool-call carries across ALL
// choices in a single streaming request. A request that emits more than this
// number of distinct (choice, tool-index) pairs is a protocol violation and
// causes fail-closed abort. 1024 is orders of magnitude above any realistic
// use-case while still bounding memory growth from adversarial inputs.
const maxToolCallsTotal = 1024

// maxToolIDLen and maxToolNameLen bound the per-key retained state to prevent
// unbounded memory growth from adversarial inputs.
const maxToolIDLen = 256
const maxToolNameLen = 256

// canonicalPseudonymRe matches the canonical pseudonym shape: PII_ followed by
// exactly 2 alphanumeric characters, then _, then exactly 24 lowercase hex
// characters (total 31 bytes). This is the shape produced by pseudonym().
// The regexp is compiled once at package load.
var canonicalPseudonymRe = regexp.MustCompile(`PII_[A-Za-z0-9]{2}_[0-9a-f]{24}`)

// isCanonicalPseudonym reports whether s contains a value matching the
// canonical pseudonym shape (31 bytes: PII_<2alnum>_<24hex>). Any occurrence
// — known to this filter or not — triggers the check; unknown canonical shapes
// must fail-closed to uphold the "no pseudonym to client" guarantee.
func isCanonicalPseudonym(s string) bool {
	return canonicalPseudonymRe.MatchString(s)
}

// residualSubCanonicalTotal counts the number of times a sub-canonical PII_
// marker (not a full canonical pseudonym shape) was observed in restored content
// text. The counter is package-level, content-free (count only), and exported
// for testing and metric collection. It is incremented atomically so it is safe
// to read concurrently from multiple goroutines.
//
// A sub-canonical marker in content is passed through (the restorer cannot
// recover what it did not anonymize, and blocking would cause false-positive
// aborts on natural text). This counter lets operators detect anomalies without
// recording any content value.
var residualSubCanonicalTotal atomic.Int64

// ResidualSubCanonicalCount returns the total number of sub-canonical PII_
// markers observed in restored content across all requests since process start.
// The value is informational only; it does not identify any specific request,
// value, or user.
func ResidualSubCanonicalCount() int64 {
	return residualSubCanonicalTotal.Load()
}

// residualScan checks whether a canonical pseudonym spans the boundary between
// previously-emitted bytes and the new fragment frag by inspecting the
// accumulated view [emittedTail + frag]. It also checks for sub-canonical PII_
// markers spanning that boundary.
//
// The tail field (a pointer to a []byte) holds the last pseudonymLen-1 (30)
// bytes emitted in the lane. On each call residualScan:
//  1. Builds check = *tail + frag (only the suffix of combined that is needed
//     for pattern matching, bounded by len(frag)+pseudonymLen-1 bytes).
//  2. If canonicalPseudonymRe matches check → fail-closed (errStreamAborted),
//     regardless of inToolArgs.
//  3. If strings.Contains(check, pseudonymMarker) and inToolArgs → fail-closed
//     (sub-canonical marker in tool arguments is always fail-closed).
//  4. If strings.Contains(check, pseudonymMarker) and !inToolArgs → increment
//     residualSubCanonicalTotal (count-only; pass through).
//  5. Updates *tail to the last min(len(check), pseudonymLen-1) bytes of check.
//
// The caller must call residualScan AFTER the Restore call, passing the
// restored fragment as frag. residualScan does not read the lane's carry.
//
// Note: step 4 may over-count sub-canonical markers when the tail window
// overlaps with the previous emission, causing a marker near an emission
// boundary to be counted twice. The counter is an approximate anomaly signal
// only — content-free and count-only — and should not be treated as an exact
// occurrence tally.
func residualScan(tail *[]byte, frag string, inToolArgs bool) error {
	// Build the combined view: prior tail bytes + new fragment.
	// We only need the last pseudonymLen-1 bytes of tail + all of frag to
	// detect a pattern that begins in tail and ends in frag.
	combined := make([]byte, 0, len(*tail)+len(frag))
	combined = append(combined, *tail...)
	combined = append(combined, frag...)

	check := string(combined)

	// Step 2: canonical pseudonym → fail-closed (both content and tool args).
	if canonicalPseudonymRe.MatchString(check) {
		return errStreamAborted
	}

	// Step 3/4: sub-canonical PII_ marker.
	if strings.Contains(check, pseudonymMarker) {
		if inToolArgs {
			return errStreamAborted
		}
		// Content: count-only, pass through.
		residualSubCanonicalTotal.Add(1)
	}

	// Step 5: update tail to last pseudonymLen-1 bytes of combined.
	const tailLen = pseudonymLen - 1
	if len(combined) <= tailLen {
		*tail = combined
	} else {
		// Take only the last tailLen bytes.
		suffix := make([]byte, tailLen)
		copy(suffix, combined[len(combined)-tailLen:])
		*tail = suffix
	}

	return nil
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

// toolCallCarry holds per-(choice, tool-call-index) state for tool_calls
// argument restoration. Each distinct tool-call index within a choice gets
// its own toolCallCarry, allocated on first observation.
type toolCallCarry struct {
	// argsCarry is the rolling buffer for function.arguments, analogous to
	// choiceCarry.carry for content. Invariant: argsCarry is either empty or a
	// proper prefix of a known pseudonym.
	argsCarry []byte
	// emittedTail holds the last min(len(emitted), pseudonymLen-1) bytes that
	// have been emitted for this tool-call's arguments lane. It is used by
	// residualScan to detect a canonical pseudonym that spans two consecutive
	// emission fragments (cross-emission boundary detection).
	emittedTail []byte
	// headerSent is true after the first tool_calls chunk for this index has
	// been emitted (which included id, type, and name).
	headerSent bool
	// id is the tool call id, stored from the first observation. Later
	// fragments must repeat the same id or omit it.
	id string
	// name is the function name, stored from the first observation. Later
	// fragments must repeat the same name or omit it.
	name string
}

// choiceCarry holds the per-choice rolling-buffer state for StreamRestorer.
type choiceCarry struct {
	// carry is the accumulated, not-yet-emitted content bytes. Invariant: carry
	// is either empty or a proper prefix of a known pseudonym — never arbitrary
	// buffered text, never an emittable fragment.
	carry []byte
	// emittedTail holds the last min(len(emitted), pseudonymLen-1) bytes that
	// have been emitted for this choice's content lane (delta.content, refusal,
	// or text). It is used by residualScan to detect a canonical pseudonym that
	// spans two consecutive emission fragments.
	emittedTail []byte
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
	// toolCalls holds per-tool-call-index carry state. Keyed by the integer
	// index from the upstream delta.tool_calls array.
	toolCalls map[int]*toolCallCarry
	// sawToolCall is true after at least one fully-validated, non-empty
	// tool-call element has been processed for this choice. Empty/null
	// tool_calls arrays do not set this flag.
	sawToolCall bool
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
// # Guarantee precision
//
// The client never receives a canonical/exact pseudonym (PII_<2 alnum>_<24
// hex>). A sub-canonical PII_-prefixed fragment in free CONTENT (e.g. "PII_X")
// may pass through: it is not a restorable pseudonym and failing closed would
// abort on natural text containing "PII_". Such fragments carry no PII — they
// are opaque character sequences that were never inserted by the anonymizer. In
// tool-call arguments, even sub-canonical PII_-prefixed fragments are
// fail-closed because tool arguments must contain only structured data (no
// natural text containing "PII_" is expected there).
//
// # Availability trade-off
//
// If the upstream response ends (via [DONE]) while any per-choice carry is
// non-empty, the restorer returns errCarryNotEmpty and aborts the stream
// fail-closed. This covers the case where natural content happens to end on a
// proper prefix of a known pseudonym (e.g. the response legitimately ends with
// a capital "P", "PI", "PII", or "PII_"). Because truncation is
// indistinguishable from a partial pseudonym at the stream boundary, the
// restorer cannot safely emit the held bytes. This is a deliberate, leak-safe
// design choice: the alternative — emitting ambiguous trailing bytes — risks
// exposing a real pseudonym fragment to the client.
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
	// totalToolCalls is the count of distinct toolCallCarry entries created
	// across all choices. Used to enforce maxToolCallsTotal.
	totalToolCalls int
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
	// Verify all carries (content and tool-call arguments) are empty across
	// all choices. A non-empty carry means the upstream truncated a pseudonym.
	for _, cc := range s.choices {
		if len(cc.carry) > 0 {
			s.aborted = true
			return nil, false, errCarryNotEmpty
		}
		for _, tc := range cc.toolCalls {
			if len(tc.argsCarry) > 0 {
				s.aborted = true
				return nil, false, errCarryNotEmpty
			}
		}
	}

	// For every choice that streamed at least one validated tool call, a
	// finish_reason must have been seen before [DONE]. OpenAI and vLLM always
	// send finish_reason before [DONE]; if it is absent the stream is truncated
	// or malformed — fail-closed. Non-tool choices are unaffected.
	for _, cc := range s.choices {
		if cc.sawToolCall && !cc.finished {
			s.aborted = true
			return nil, false, errStreamAborted
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

// rawToolCallFunction is the typed representation of the function field inside
// a tool_calls delta element.
//
// Name is a *string because sonic (the JSON library) correctly distinguishes
// absent string key (nil) from explicit string value (non-nil).
//
// Arguments is parsed separately from the raw function map (not via a struct
// tag) because sonic maps JSON null → nil *string, making it impossible to
// distinguish `"arguments": null` from `"arguments"` being absent when both
// are represented as *string. Instead, the validation loop in
// handleToolCallsDelta checks the raw function JSON map directly: if the
// "arguments" key is present, its value must be a JSON string; null or any
// non-string type causes fail-closed abort.
type rawToolCallFunction struct {
	Name *string `json:"name"`
	// Arguments is intentionally absent as a struct field; it is read from the
	// raw function map in handleToolCallsDelta to support null detection.
}

// rawToolCall is the typed representation of one element in delta.tool_calls.
// The Function field holds the raw JSON bytes of the function object so that
// handleToolCallsDelta can parse it both as a typed struct (for Name) and as a
// raw key-presence map (for Arguments null-vs-absent detection).
type rawToolCall struct {
	Index    *int              `json:"index"`
	ID       *string           `json:"id"`
	Type     *string           `json:"type"`
	Function *jsonx.RawMessage `json:"function"`
}

// handleChoices processes a non-empty choices array from one upstream SSE chunk.
func (s *StreamRestorer) handleChoices(rawChoices []jsonx.RawMessage) ([][]byte, bool, error) {
	// Parse choices as typed structs.
	type rawDelta struct {
		Role    *string `json:"role"`
		Content *string `json:"content"`
		Refusal *string `json:"refusal"`
		// ToolCalls is parsed separately via rawDeltaMap for key-presence detection.
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
		hasDeltaToolCalls := false
		var contentStr string
		var rawToolCallsJSON jsonx.RawMessage
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

			// Audit every delta key. Known content fields: content, refusal,
			// tool_calls. Legacy function_call stays fail-closed. Unknown fields
			// are conservatively dropped (envelope is synthesized, so unknown
			// fields cannot leak pseudonyms).
			for k, v := range rawDeltaMap {
				switch k {
				case "role", "content", "refusal":
					// Explicitly handled fields — fall through.
				case "tool_calls":
					// tool_calls is handled below; capture raw JSON for typed parsing.
					hasDeltaToolCalls = true
					rawToolCallsJSON = v
				case "function_call":
					// Legacy singular delta.function_call: always fail-closed.
					s.aborted = true
					return nil, false, errStreamAborted
				}
				// All other unrecognised delta fields are conservatively ignored.
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
		activeContentFields := 0
		if hasDeltaContent {
			activeContentFields++
		}
		if hasDeltaRefusal {
			activeContentFields++
		}
		if hasTextContent {
			activeContentFields++
		}
		if activeContentFields > 1 {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// tool_calls must not appear alongside content fields in the same chunk.
		if hasDeltaToolCalls && activeContentFields > 0 {
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

		// Fail-closed: content or tool_calls after finish_reason.
		if cc.finished && (activeContentFields > 0 || hasDeltaToolCalls) {
			s.aborted = true
			return nil, false, errStreamAborted
		}

		// Format guard: tool_calls only valid when format is unknown or chat.
		// If format is already locked to completion or refusal, tool_calls is
		// a protocol violation.
		if hasDeltaToolCalls {
			switch cc.format {
			case formatUnknown, formatChat:
				// Valid.
			default:
				s.aborted = true
				return nil, false, errStreamAborted
			}
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

		// Cross-lane ownership within a choice: a pseudonym always lies entirely
		// within one lane (content-kind or tool-index) of a given choice.
		// While the content carry for this choice is non-empty, emitting from a
		// tool-call lane of the SAME choice is fail-closed, and vice versa.
		// Across different choices, independent carries are expected and valid
		// (multiple choices may stream content simultaneously in separate lanes).
		// Validate cross-lane constraints BEFORE processing so the operation is
		// transactional: detect violations before emitting anything.
		if hasDeltaToolCalls {
			if err := s.checkCrossLane(choiceIdx, true, nil); err != nil {
				s.aborted = true
				return nil, false, err
			}
		}

		// Reverse cross-lane check: when emitting content for this choice, verify
		// that no tool-call lane of the SAME choice holds a non-empty argsCarry.
		// This is the symmetric rule to checkCrossLane(emittingTool=true): if a
		// tool argument carry is live, a content delta in the same choice is a
		// cross-lane violation. Cross-CHOICE carries are independent and must not
		// block each other.
		if activeContentFields > 0 && cc.toolCalls != nil {
			for _, tc := range cc.toolCalls {
				if len(tc.argsCarry) > 0 {
					s.aborted = true
					return nil, false, errStreamAborted
				}
			}
		}

		// Process content through the rolling buffer.
		if activeContentFields > 0 {
			emitLines, err := s.pushContent(choiceIdx, cc, contentStr)
			if err != nil {
				s.aborted = true
				return nil, false, err
			}
			out = append(out, emitLines...)
		}

		// Process tool_calls delta.
		if hasDeltaToolCalls {
			toolLines, err := s.handleToolCallsDelta(choiceIdx, cc, rawToolCallsJSON)
			if err != nil {
				s.aborted = true
				return nil, false, err
			}
			out = append(out, toolLines...)
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
			// Additional gate: "tool_calls" finish_reason requires that at least
			// one fully-validated tool-call element was processed for this choice.
			// Without this gate, the Anthropic adapter's translated tool_use→tool_calls
			// finish_reason would slip through even though sawToolCall is false.
			if reason == "tool_calls" && !cc.sawToolCall {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			// Conversely: a choice that processed tool calls must finish with
			// "tool_calls", not silently accept "stop" or any other reason.
			if cc.sawToolCall && reason != "tool_calls" {
				s.aborted = true
				return nil, false, errStreamAborted
			}
			cc.finished = true
			// All carries for this choice must be empty at finish time.
			if len(cc.carry) > 0 {
				s.aborted = true
				return nil, false, errCarryNotEmpty
			}
			for _, tc := range cc.toolCalls {
				if len(tc.argsCarry) > 0 {
					s.aborted = true
					return nil, false, errCarryNotEmpty
				}
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

// checkCrossLane enforces the within-choice cross-lane carry ownership invariant.
// A pseudonym always lies entirely within one lane (content-kind or tool-index)
// of a given choice. While the content carry for a choice is non-empty, emitting
// from a tool-call lane of the SAME choice must fail-closed, and vice versa.
// Across different choices, independent carries are expected and valid.
//
// emittingTool must be true when the caller is processing a tool_calls delta for
// choiceIdx. toolIndices (if non-nil) lists the tool indices being written in
// this chunk; any OTHER tool index for the same choice with a non-empty carry is
// a cross-lane violation.
//
// This function only detects cross-lane violations within choiceIdx. Intra-lane
// content continuation (same choice, same field kind) is always allowed.
func (s *StreamRestorer) checkCrossLane(choiceIdx int, emittingTool bool, toolIndices []int) error {
	cc, ok := s.choices[choiceIdx]
	if !ok {
		return nil
	}
	if emittingTool {
		// Content carry and tool carry are different lanes within the same choice.
		// If the content carry is non-empty, emitting a tool_calls delta is
		// cross-lane — fail-closed.
		if len(cc.carry) > 0 {
			return errStreamAborted
		}
		// If emitting for specific tool indices, verify no OTHER tool index for
		// this choice holds a non-empty argsCarry.
		if len(toolIndices) > 0 && len(cc.toolCalls) > 0 {
			emittingSet := make(map[int]struct{}, len(toolIndices))
			for _, ti := range toolIndices {
				emittingSet[ti] = struct{}{}
			}
			for ti, tc := range cc.toolCalls {
				if _, active := emittingSet[ti]; active {
					continue
				}
				if len(tc.argsCarry) > 0 {
					return errStreamAborted
				}
			}
		}
	}
	return nil
}

// parsedToolElement holds the fully-decoded fields of one tool_calls delta
// element after validation. It is populated during the validation pass in
// handleToolCallsDelta and consumed during the emit pass.
type parsedToolElement struct {
	toolIdx int
	// argsFragment is the decoded Go string value of the arguments field.
	// hasArgs is true when the arguments key was present in the JSON; when
	// false, argsFragment is the zero string and argsRaw is nil.
	hasArgs      bool
	argsFragment string
}

// handleToolCallsDelta processes the raw JSON of a delta.tool_calls array for
// the given choice. It parses each element, validates the header state machine,
// applies the rolling-buffer hold-back to function.arguments, and emits
// synthesized tool_calls chunks.
func (s *StreamRestorer) handleToolCallsDelta(choiceIdx int, cc *choiceCarry, rawJSON jsonx.RawMessage) ([][]byte, error) {
	// Validate that the value is a JSON array, not null or any other type.
	// A null tool_calls key ("tool_calls": null) is a protocol violation:
	// key presence signals tool-call intent, and null is not an array.
	// An empty array ("tool_calls": []) is similarly a protocol violation:
	// the upstream is signaling tool-call intent with no actual tool calls.
	// Both cases are fail-closed to prevent ambiguity.
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errStreamAborted
	}

	// Parse as array — fail-closed if unparseable.
	var elements []rawToolCall
	if err := jsonx.Unmarshal(rawJSON, &elements); err != nil {
		return nil, errStreamAborted
	}

	// An empty array is a protocol violation (see comment above).
	if len(elements) == 0 {
		return nil, errStreamAborted
	}

	// Enforce per-choice distinct-tool-call cap.
	if cc.toolCalls == nil {
		cc.toolCalls = make(map[int]*toolCallCarry)
	}

	// Validate all elements before emitting anything (transactional).
	// seenInChunk detects duplicate indices within THIS chunk's tool_calls array.
	seenInChunk := make(map[int]struct{}, len(elements))
	// toolIndicesInChunk collects all tool indices referenced in this chunk
	// for the cross-lane ownership check.
	toolIndicesInChunk := make([]int, 0, len(elements))
	// parsed holds the per-element decoded state built during validation and
	// used in the emit pass to avoid re-parsing.
	parsed := make([]parsedToolElement, len(elements))

	for i := range elements {
		el := &elements[i]
		// Index is required.
		if el.Index == nil {
			return nil, errStreamAborted
		}
		toolIdx := *el.Index
		if toolIdx < 0 || toolIdx >= maxToolCallsPerChoice {
			return nil, errStreamAborted
		}
		// Duplicate index within this chunk is a protocol violation.
		if _, dup := seenInChunk[toolIdx]; dup {
			return nil, errStreamAborted
		}
		seenInChunk[toolIdx] = struct{}{}
		toolIndicesInChunk = append(toolIndicesInChunk, toolIdx)
		parsed[i].toolIdx = toolIdx

		// Parse the function field as both a typed struct (for Name) and a raw
		// key-presence map (for arguments null-vs-absent detection). Both parses
		// target the same bytes; the raw map parse is authoritative for argument
		// validation because sonic maps JSON null → nil *string, making a *string
		// Arguments field unable to distinguish null from absent.
		var fnName *string
		var fnRawMap map[string]jsonx.RawMessage
		if el.Function != nil {
			var fnStruct rawToolCallFunction
			if err := jsonx.Unmarshal(*el.Function, &fnStruct); err != nil {
				return nil, errStreamAborted
			}
			fnName = fnStruct.Name
			if err := jsonx.Unmarshal(*el.Function, &fnRawMap); err != nil {
				return nil, errStreamAborted
			}
		}

		tc, exists := cc.toolCalls[toolIdx]
		if !exists {
			// First observation of this index: enforce per-choice cap.
			if len(cc.toolCalls) >= maxToolCallsPerChoice {
				return nil, errStreamAborted
			}
			// Enforce global tool-call cap across all choices.
			if s.totalToolCalls >= maxToolCallsTotal {
				return nil, errStreamAborted
			}
			// First observation requires non-empty id, type=="function", function.name.
			if el.ID == nil || *el.ID == "" {
				return nil, errStreamAborted
			}
			if el.Type == nil || *el.Type != "function" {
				return nil, errStreamAborted
			}
			if fnName == nil || *fnName == "" {
				return nil, errStreamAborted
			}
			id := *el.ID
			name := *fnName
			// Validate id: must use only the conservative charset [A-Za-z0-9_.+-]
			// and must not contain a PII_ marker or canonical pseudonym shape.
			// Characters outside the charset (e.g. '@', spaces, ':', non-ASCII) can
			// carry PII and are always fail-closed. Length is capped at maxToolIDLen.
			if len(id) > maxToolIDLen {
				return nil, errStreamAborted
			}
			if !toolIDCharsetRe.MatchString(id) {
				return nil, errStreamAborted
			}
			if strings.Contains(id, pseudonymMarker) || isCanonicalPseudonym(id) {
				return nil, errStreamAborted
			}
			// Validate name: same conservative charset and PII checks.
			if len(name) > maxToolNameLen {
				return nil, errStreamAborted
			}
			if !toolNameCharsetRe.MatchString(name) {
				return nil, errStreamAborted
			}
			if strings.Contains(name, pseudonymMarker) || isCanonicalPseudonym(name) {
				return nil, errStreamAborted
			}
			tc = &toolCallCarry{id: id, name: name}
			cc.toolCalls[toolIdx] = tc
			s.totalToolCalls++
		} else {
			// Subsequent fragment: id/type/name must be absent (nil pointer) or an
			// exact repeat of the stored value. A present-but-empty string (non-nil
			// pointer to "") is "present but empty" and is treated as a protocol
			// violation — fail-closed — because a present key with an empty value is
			// structurally anomalous and distinct from a legitimately absent key.
			if el.ID != nil {
				if *el.ID == "" || *el.ID != tc.id {
					return nil, errStreamAborted
				}
			}
			if el.Type != nil {
				if *el.Type == "" || *el.Type != "function" {
					return nil, errStreamAborted
				}
			}
			if fnName != nil {
				if *fnName == "" || *fnName != tc.name {
					return nil, errStreamAborted
				}
			}
		}

		// Validate the arguments field. Use the raw function map to detect
		// presence and type of the "arguments" key:
		//   - key absent:          header-only fragment — valid, no arguments processing.
		//   - key present + null:  fail-closed (JSON null is not a valid string).
		//   - key present + string: decode and use as the fragment.
		//   - key present + other (number, object, array, bool): fail-closed.
		if rawArgsVal, argsKeyPresent := fnRawMap["arguments"]; argsKeyPresent {
			trimmedArgs := bytes.TrimSpace(rawArgsVal)
			// JSON null → fail-closed.
			if bytes.Equal(trimmedArgs, []byte("null")) {
				return nil, errStreamAborted
			}
			// Must be a JSON string (starts with '"').
			if len(trimmedArgs) < 2 || trimmedArgs[0] != '"' {
				return nil, errStreamAborted
			}
			// Unmarshal as string to confirm it is a valid JSON string value.
			var argStr string
			if err := jsonx.Unmarshal(rawArgsVal, &argStr); err != nil {
				return nil, errStreamAborted
			}
			parsed[i].hasArgs = true
			parsed[i].argsFragment = argStr
		}
	}

	// Cross-lane check: verify that no OTHER tool index for this choice has a
	// non-empty carry while we are emitting for toolIndicesInChunk.
	if err := s.checkCrossLane(choiceIdx, true, toolIndicesInChunk); err != nil {
		return nil, err
	}

	// Now emit: all validation above passed.
	var out [][]byte
	for i := range elements {
		toolIdx := parsed[i].toolIdx
		tc := cc.toolCalls[toolIdx]

		// Only process arguments if the key was present in the JSON.
		if !parsed[i].hasArgs {
			// Header-only fragment (arguments key absent): emit header chunk if not yet sent.
			if !tc.headerSent {
				line, err := s.buildToolCallChunk(choiceIdx, toolIdx, cc, tc, "")
				if err != nil {
					return nil, errStreamAborted
				}
				out = append(out, line, nil)
				tc.headerSent = true
				cc.roleSent = true
				cc.sawToolCall = true
			}
			continue
		}

		fragment := parsed[i].argsFragment

		// Append fragment to argsCarry.
		before := len(tc.argsCarry)
		tc.argsCarry = append(tc.argsCarry, fragment...)
		s.totalCarryBytes += len(tc.argsCarry) - before

		if s.totalCarryBytes > maxPIIStreamBytes {
			return nil, errStreamAborted
		}

		// Apply the same hold-back as content.
		L := s.longestPseudonymPrefixSuffix(tc.argsCarry)
		B := len(tc.argsCarry) - L

		if B <= 0 {
			// Nothing safe to emit yet; emit header if not yet sent.
			if !tc.headerSent {
				line, err := s.buildToolCallChunk(choiceIdx, toolIdx, cc, tc, "")
				if err != nil {
					return nil, errStreamAborted
				}
				out = append(out, line, nil)
				tc.headerSent = true
				cc.roleSent = true
				cc.sawToolCall = true
			}
			continue
		}

		safe := tc.argsCarry[:B]
		restored := string(s.filter.Restore(safe))

		// Residual scan: detect a canonical or sub-canonical PII_ token spanning
		// the boundary between the last emission and this fragment. Both canonical
		// and sub-canonical tokens in tool arguments are fail-closed.
		if err := residualScan(&tc.emittedTail, restored, true); err != nil {
			return nil, err
		}

		// Advance carry.
		carry := tc.argsCarry[B:]
		tc.argsCarry = make([]byte, len(carry))
		copy(tc.argsCarry, carry)
		s.totalCarryBytes -= B

		line, err := s.buildToolCallChunk(choiceIdx, toolIdx, cc, tc, restored)
		if err != nil {
			return nil, errStreamAborted
		}
		out = append(out, line, nil)
		tc.headerSent = true
		cc.roleSent = true
		cc.sawToolCall = true
	}

	return out, nil
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

	// Residual scan: detect a canonical or sub-canonical PII_ token spanning
	// the boundary between the last emission and this fragment. For content:
	// canonical tokens → fail-closed; sub-canonical → count-only, pass through.
	restoredStr := string(restored)
	if err := residualScan(&cc.emittedTail, restoredStr, false); err != nil {
		return nil, err
	}

	// Build SSE output lines.
	line, err := s.buildContentChunk(choiceIdx, cc, restoredStr)
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

// sseToolCallFunction is the typed function field inside a synthesized tool_calls delta.
type sseToolCallFunction struct {
	// Name is emitted only on the first chunk for a given tool-call index.
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// sseToolCallElement is one element of the tool_calls array in a synthesized delta.
// ID and Type are emitted only on the first chunk for a given tool-call index
// (headerSent=false before emission); subsequent chunks omit them.
type sseToolCallElement struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function sseToolCallFunction `json:"function"`
}

// sseDeltaToolCalls is the typed delta for a tool_calls chunk.
type sseDeltaToolCalls struct {
	Role      string               `json:"role,omitempty"`
	ToolCalls []sseToolCallElement `json:"tool_calls"`
}

// sseToolCallsChoice is a typed chat-completion choice carrying a tool_calls delta.
type sseToolCallsChoice struct {
	Index        int               `json:"index"`
	Delta        sseDeltaToolCalls `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
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

// buildToolCallChunk builds a data: SSE line carrying a tool_calls delta for
// the given (choice index, tool-call index). The arguments field is set to the
// already-restored string fragment (may be empty for header-only chunks). The
// envelope (id, object, created, model) is synthesized locally — never copied
// from the upstream response — to prevent pseudonym echo.
//
// On the first chunk for a given tool-call index (tc.headerSent==false), the
// id, type, and function.name fields are emitted. On subsequent chunks they are
// omitted (empty omitempty fields). The arguments fragment is always included,
// even when empty, so the client receives complete JSON framing.
//
// Role synthesis: if this is the first emitted chunk for the choice
// (cc.roleSent==false), the synthesized role "assistant" is included in the
// delta — regardless of whether the upstream sent a role field. The upstream
// role value is never echoed. Subsequent chunks omit the role (cc.roleSent is
// set to true by the caller immediately after this function returns).
func (s *StreamRestorer) buildToolCallChunk(choiceIdx int, toolIdx int, cc *choiceCarry, tc *toolCallCarry, argsFragment string) ([]byte, error) {
	// Build the tool call element.
	elem := sseToolCallElement{
		Index: toolIdx,
		Function: sseToolCallFunction{
			Arguments: argsFragment,
		},
	}
	if !tc.headerSent {
		// Emit header fields only on the first chunk for this tool-call index.
		elem.ID = tc.id
		elem.Type = "function"
		elem.Function.Name = tc.name
	}

	// Synthesize "assistant" role on the first emitted chunk for this choice.
	// The upstream role value is never used — we always synthesize "assistant".
	// This ensures tool-only streams deliver role:"assistant" to the client on
	// the very first chunk, consistent with content streams.
	role := ""
	if !cc.roleSent {
		role = "assistant"
	}

	delta := sseDeltaToolCalls{
		Role:      role,
		ToolCalls: []sseToolCallElement{elem},
	}

	env := sseEnvelope{
		ID:      s.chunkID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.modelName,
		Choices: []sseToolCallsChoice{{
			Index:        choiceIdx,
			Delta:        delta,
			FinishReason: nil,
		}},
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
