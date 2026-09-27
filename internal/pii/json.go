package pii

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// knownContentPartTypes is the set of non-text content-part type values that
// carry no textual content and should be skipped (not scanned, not rejected).
// Any type value NOT in this set and NOT "text" triggers fail-closed.
var knownContentPartTypes = map[string]bool{
	"image_url":   true,
	"input_audio": true,
	"input_image": true,
	"file":        true,
	"image":       true,
	"audio":       true,
	"document":    true,
	"video":       true,
}

// coveredTopLevelFields lists the top-level request-body fields that
// anonymizeWithDetectors handles with dedicated shape validation above (user,
// stop, prompt, input, tools, messages, logit_bias, tool_choice). They are
// excluded from the default unknown-field scan to avoid processing them
// twice.
var coveredTopLevelFields = map[string]bool{
	"user":        true,
	"stop":        true,
	"prompt":      true,
	"input":       true,
	"tools":       true,
	"messages":    true,
	"logit_bias":  true,
	"tool_choice": true,
}

// exemptTopLevelFields lists structural top-level request-body fields whose
// value must remain byte-identical for routing semantics to keep working —
// a pseudonymized "model" would break routing to the correct upstream — and
// which are skipped entirely, with no shape validation, because their value
// is never free text. Every top-level field NOT in this set and NOT in
// coveredTopLevelFields is treated as potential content and scanned by
// default. Fields whose exemption depends on their value having a specific
// shape (logit_bias, tool_choice) are handled with dedicated shape
// validation above instead of living in this set — see
// coveredTopLevelFields.
var exemptTopLevelFields = map[string]bool{
	"model": true,
}

// hasDuplicateKeys reports whether body contains a JSON object (at any level of
// nesting) that has at least one duplicated key. It uses encoding/json's
// token-streaming decoder (stdlib, no CGO). Duplicate keys are rejected because
// JSON decoders that retain the first value rather than the last (or vice-versa)
// can disagree on the effective content, enabling smuggling attacks where PII
// appears in a first-occurrence key that the map-based scanner does not see but
// the upstream LLM does.
//
// Returns (true, nil) when a duplicate is found, (false, nil) on a clean
// document, and (false, err) when the token stream is malformed (the caller
// should treat this as fail-closed regardless).
func hasDuplicateKeys(body []byte) (bool, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	return scanForDuplicateKeys(dec)
}

// scanForDuplicateKeys reads tokens from dec to walk a single JSON value and
// returns true on the first duplicate object key found at any nesting depth.
// It is called recursively for nested objects and array elements.
func scanForDuplicateKeys(dec *json.Decoder) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}

	delim, ok := tok.(json.Delim)
	if !ok {
		// Scalar value (string, number, bool, null): no keys to check.
		return false, nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			// Read the key token.
			keyTok, err := dec.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return false, errors.New("expected string key in JSON object")
			}
			if _, exists := seen[key]; exists {
				return true, nil
			}
			seen[key] = struct{}{}
			// Recurse into the value.
			dup, err := scanForDuplicateKeys(dec)
			if err != nil {
				return false, err
			}
			if dup {
				return true, nil
			}
		}
		// Consume the closing '}'.
		if _, err := dec.Token(); err != nil {
			return false, err
		}

	case '[':
		for dec.More() {
			dup, err := scanForDuplicateKeys(dec)
			if err != nil {
				return false, err
			}
			if dup {
				return true, nil
			}
		}
		// Consume the closing ']'.
		if _, err := dec.Token(); err != nil {
			return false, err
		}
	}

	return false, nil
}

// anonymizeWithDetectors replaces PII in all PII-bearing string fields of an
// OpenAI-shaped request body. It handles chat completion, legacy completion,
// embeddings, and rerank/score request shapes. Explicitly covered fields (each
// with dedicated shape validation):
//
// Chat completions:
//   - messages[].content (string or array-of-parts "text" field)
//   - messages[].name
//   - messages[].tool_calls[].function.arguments (JSON string, scanned as text)
//   - messages[].function_call.arguments (legacy, JSON string, scanned as text)
//   - tools[].function.description
//   - tools[].function.parameters: string leaf values only (description, default,
//     enum strings, title, etc.); object structure and keys are never modified.
//   - top-level "user"
//
// Completions (legacy):
//   - top-level "prompt" (string or array-of-strings)
//
// Embeddings:
//   - top-level "input" (string or array-of-strings; array-of-ints/token-arrays left unchanged)
//
// All request shapes:
//   - top-level "stop" (string or array-of-strings; each string is
//     pseudonymized independently, never concatenated)
//   - top-level "logit_bias": exempt (untouched) only when it is a JSON
//     object whose keys are all 1-to-7-digit ASCII-digit strings (token
//     IDs — tokenizer vocabularies stay below 10 million entries) and whose
//     values are all JSON numbers, or is JSON null. Any other shape,
//     including a longer digit string that could carry a card or phone
//     number, is rejected fail-closed — the value is never scanned, because
//     a pseudonymized token-ID key would corrupt the bias map.
//   - top-level "tool_choice": a JSON string is scanned like any other
//     string field; JSON null is a no-op; a JSON object's
//     "function.name" is validated against the charset OpenAI restricts
//     function names to (^[A-Za-z0-9_-]{1,64}$) and left untouched — every
//     other string leaf in the object is scanned normally via
//     scanStringLeaves. Any other shape is rejected fail-closed.
//
// Every other top-level key — anything not listed above — is scanned by
// default: it is treated as potential free-text content and passed through
// scanStringLeaves, which pseudonymizes every string leaf independently
// (never concatenating array elements or object fields), leaves numbers,
// booleans, and null untouched, rejects PII found in an object key
// fail-closed, and bounds recursion at maxScanDepth. This covers rerank and
// score request fields (e.g. "query", "documents", "texts", "text_1",
// "text_2", "queries", "items", "instruction"), sampling and shape knobs
// that carry no routing semantics (e.g. "reasoning_effort", "service_tier",
// "modalities", "stream_options"), and any current or future
// provider-specific field (e.g. vLLM's "chat_template_kwargs") without
// endpoint-specific code — no client-supplied text reaches an upstream
// unscanned through any top-level field. Only "model" (see
// exemptTopLevelFields) is exempt from this default scan unconditionally,
// because its value must stay byte-identical for routing.
//
// detectors are called for each string value to locate PII spans. replace
// is called once per unique (type, originalValue) to obtain the pseudonym;
// it returns an error if the per-request mapping cap is exceeded.
//
// Fail-closed: returns an error when the body cannot be parsed, any covered
// field is present but has an unexpected type/shape, any field cannot be
// re-serialized, or replace returns an error. Error messages never contain
// body content or PII values.
func anonymizeWithDetectors(body []byte, detectors []Detector, replace func(typ, value string) (string, error)) ([]byte, error) {
	detect := func(text string) (string, bool, error) {
		var spans []Span
		for _, d := range detectors {
			found, err := d.Find(text)
			if err != nil {
				return "", false, err
			}
			spans = append(spans, found...)
		}
		if len(spans) == 0 {
			return text, false, nil
		}
		// Sort and de-overlap merged spans from all detectors.
		// Primary: Start ascending. Secondary: End descending (longest first).
		// Tertiary: Type ascending — ensures fully-identical intervals (same
		// Start and End, different Type) always produce the same winner in
		// deOverlap regardless of detector ordering or input order, making
		// multi-turn pseudonym assignment fully deterministic.
		sort.Slice(spans, func(i, j int) bool {
			if spans[i].Start != spans[j].Start {
				return spans[i].Start < spans[j].Start
			}
			if spans[i].End != spans[j].End {
				return spans[i].End > spans[j].End // longest first on tie
			}
			return spans[i].Type < spans[j].Type // stable type tie-break
		})
		spans = deOverlap(spans)
		out, touched, err := replaceSpansInText(text, spans, replace)
		return out, touched, err
	}

	// Fix #3: reject any body that contains duplicate JSON object keys anywhere
	// in the document (recursive, all nesting levels). A body with duplicate keys
	// can be parsed differently by different JSON implementations — the map-based
	// scan below sees only the last value for each key, while some upstream servers
	// retain the first. This creates a smuggling window where PII appears in an
	// earlier duplicate that the scanner does not see. Fail-closed here eliminates
	// the window; the !touched original-return path is also safe because dup-key
	// bodies are rejected before reaching it.
	if dup, dupErr := hasDuplicateKeys(body); dupErr != nil || dup {
		return nil, errors.New("pii: request body could not be processed for anonymization")
	}

	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil || doc == nil {
		// A JSON null body or a non-object body cannot be scanned: fail-closed.
		return nil, errors.New("pii: request body could not be processed for anonymization")
	}

	touched := false

	// ── top-level "user" field ──────────────────────────────────────────────
	// Fail-closed: when "user" is present but is not a string, reject rather
	// than silently forwarding unscanned content (e.g. an object or array).
	if rawUser, ok := doc["user"]; ok {
		var userStr string
		if err := jsonx.Unmarshal(rawUser, &userStr); err != nil {
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}
		replaced, did, err := detect(userStr)
		if err != nil {
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}
		if did {
			newJSON, err := jsonx.Marshal(replaced)
			if err != nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			doc["user"] = jsonx.RawMessage(newJSON)
			touched = true
		}
	}

	// ── top-level "stop" field ───────────────────────────────────────────────
	// Applies to every provider: "stop" is part of the OpenAI request surface
	// regardless of endpoint. It may be a plain string, an array of strings,
	// or JSON null (OpenAI's default, meaning "no stop sequences"); null is a
	// no-op, not a covered-field violation. Unlike "prompt" and "input", an
	// array element that is not a string (e.g. a token ID) is unsupported —
	// "stop" has no token-array variant — and is rejected fail-closed. Each
	// string is pseudonymized independently; array elements are never
	// concatenated.
	if rawStop, ok := doc["stop"]; ok {
		if !bytes.Equal(bytes.TrimSpace(rawStop), []byte("null")) {
			var stopStr string
			if err := jsonx.Unmarshal(rawStop, &stopStr); err == nil {
				replaced, did, err := detect(stopStr)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if did {
					newJSON, err := jsonx.Marshal(replaced)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					doc["stop"] = jsonx.RawMessage(newJSON)
					touched = true
				}
			} else {
				// Not a string: try array of strings.
				var stopArr []jsonx.RawMessage
				if err2 := jsonx.Unmarshal(rawStop, &stopArr); err2 == nil {
					arrTouched := false
					for i, elem := range stopArr {
						// Check the raw token explicitly rather than unmarshaling
						// straight into a string: unmarshaling a JSON null element
						// into a non-pointer string silently zeroes it to "" instead
						// of erroring, which would let a null element pass through
						// as if it were an (empty) string. Requiring the token to
						// start with '"' rejects null and every other non-string
						// element (number, object, array, bool) fail-closed.
						trimmed := bytes.TrimSpace(elem)
						if len(trimmed) == 0 || trimmed[0] != '"' {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						var s string
						if err := jsonx.Unmarshal(elem, &s); err != nil {
							// Non-string element: "stop" has no token-ID array variant
							// like "prompt"/"input" — unsupported shape → fail-closed.
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						replaced, did, err := detect(s)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if did {
							newJSON, err := jsonx.Marshal(replaced)
							if err != nil {
								return nil, errors.New("pii: request body could not be processed for anonymization")
							}
							stopArr[i] = jsonx.RawMessage(newJSON)
							arrTouched = true
						}
					}
					if arrTouched {
						newJSON, err := jsonx.Marshal(stopArr)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						doc["stop"] = jsonx.RawMessage(newJSON)
						touched = true
					}
				} else {
					// "stop" is present but is neither a string, an array, nor null:
					// unsupported shape for a covered field → fail-closed.
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
			}
		}
	}

	// ── top-level "logit_bias" field ─────────────────────────────────────────
	// logit_bias maps token IDs (numeric-string object keys) to a bias value
	// applied by the upstream sampler. The keys are structural (a token ID,
	// not text) and must remain byte-identical for the bias to apply to the
	// intended token; rewriting a key would silently corrupt sampling. JSON
	// null is OpenAI's "no bias" default and is a no-op. A key must be 1 to
	// maxLogitBiasKeyDigits ASCII digits — tokenizer vocabularies stay below
	// 10 million entries, so a legitimate token ID never needs more digits
	// than that, and a longer digit string could be smuggling a card or
	// phone number through this field instead. Any shape that is not an
	// object of such keys mapped to JSON numbers is rejected fail-closed
	// rather than scanned or forwarded.
	if rawLogitBias, ok := doc["logit_bias"]; ok {
		if !bytes.Equal(bytes.TrimSpace(rawLogitBias), []byte("null")) {
			var logitBias map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawLogitBias, &logitBias); err != nil || logitBias == nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			for key, val := range logitBias {
				if !isLogitBiasTokenIDKey(key) || !isJSONNumberLiteral(val) {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
			}
			// Valid shape: left untouched, byte-identical — never scanned.
		}
	}

	// ── top-level "tool_choice" field ────────────────────────────────────────
	// tool_choice selects how the model should call tools. A plain JSON
	// string ("auto", "none", "required", or any other value) carries no
	// routing semantics beyond its literal value and is scanned like any
	// other string field. JSON null is OpenAI's "unset" default and is a
	// no-op. An object selects a specific function: its "function.name" is a
	// structural identifier the upstream must receive byte-identical to
	// route the call correctly, so it is validated against the charset
	// OpenAI restricts function names to and left untouched — never passed
	// to detect — while every other string leaf in the object (including
	// "type" and any sibling of "function") is potential free text and is
	// scanned normally via scanStringLeaves. Any other shape (number, bool,
	// array) is rejected fail-closed.
	if rawToolChoice, ok := doc["tool_choice"]; ok {
		if !bytes.Equal(bytes.TrimSpace(rawToolChoice), []byte("null")) {
			var toolChoiceStr string
			if err := jsonx.Unmarshal(rawToolChoice, &toolChoiceStr); err == nil {
				replaced, did, err := detect(toolChoiceStr)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if did {
					newJSON, err := jsonx.Marshal(replaced)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					doc["tool_choice"] = jsonx.RawMessage(newJSON)
					touched = true
				}
			} else {
				var toolChoice map[string]jsonx.RawMessage
				if err2 := jsonx.Unmarshal(rawToolChoice, &toolChoice); err2 != nil || toolChoice == nil {
					// Neither a string, an object, nor null: unsupported shape → fail-closed.
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}

				tcTouched := false
				tcKeys := make([]string, 0, len(toolChoice))
				for k := range toolChoice {
					tcKeys = append(tcKeys, k)
				}
				sort.Strings(tcKeys)

				for _, k := range tcKeys {
					if k == "function" {
						continue
					}
					scanned, did, err := scanStringLeaves(toolChoice[k], detect)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					if did {
						toolChoice[k] = scanned
						tcTouched = true
					}
				}

				if rawFn, hasFn := toolChoice["function"]; hasFn {
					var fn map[string]jsonx.RawMessage
					if err := jsonx.Unmarshal(rawFn, &fn); err != nil || fn == nil {
						// tool_choice.function present but not an object (or is JSON null): unsupported shape.
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}

					if rawName, hasName := fn["name"]; hasName {
						var name string
						if err := jsonx.Unmarshal(rawName, &name); err != nil {
							// "name" present but is not a string: unsupported shape.
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if !toolChoiceFunctionNamePattern.MatchString(name) {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						// Valid name: left untouched, never passed to detect.
					}

					fnTouched := false
					fnKeys := make([]string, 0, len(fn))
					for k := range fn {
						if k == "name" {
							continue
						}
						fnKeys = append(fnKeys, k)
					}
					sort.Strings(fnKeys)

					for _, k := range fnKeys {
						scanned, did, err := scanStringLeaves(fn[k], detect)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if did {
							fn[k] = scanned
							fnTouched = true
						}
					}

					if fnTouched {
						newFnJSON, err := jsonx.Marshal(fn)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						toolChoice["function"] = jsonx.RawMessage(newFnJSON)
						tcTouched = true
					}
				}

				if tcTouched {
					newTCJSON, err := jsonx.Marshal(toolChoice)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					doc["tool_choice"] = jsonx.RawMessage(newTCJSON)
					touched = true
				}
			}
		}
	}

	// ── legacy completion "prompt" field ────────────────────────────────────
	// /v1/completions carries text in the top-level "prompt" field, which may
	// be a string, string[], int[], or int[][] (token arrays). String elements
	// are scanned for PII; non-string elements (token IDs, token-ID arrays) are
	// PII-free and passed through unchanged. When "prompt" is present but is
	// neither a string nor an array, its shape is unsupported for a covered
	// field — reject the request (fail-closed) rather than forwarding unscanned
	// content. This mirrors the "input" handling for embeddings exactly.
	if rawPrompt, ok := doc["prompt"]; ok {
		// Try string prompt first.
		var promptStr string
		if err := jsonx.Unmarshal(rawPrompt, &promptStr); err == nil {
			replaced, did, err := detect(promptStr)
			if err != nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			if did {
				newJSON, err := jsonx.Marshal(replaced)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				doc["prompt"] = jsonx.RawMessage(newJSON)
				touched = true
			}
		} else {
			// Not a string: try array.
			var promptArr []jsonx.RawMessage
			if err2 := jsonx.Unmarshal(rawPrompt, &promptArr); err2 == nil {
				arrTouched := false
				for i, elem := range promptArr {
					// Each element may be a string, an integer (token ID), or an
					// integer array (token-ID array, int[][]). Scan string elements
					// for PII; leave integer and integer-array elements untouched.
					// Fail-closed on any other shape (object, bool, null) — those
					// are not valid OpenAI prompt element types.
					var s string
					if err := jsonx.Unmarshal(elem, &s); err == nil {
						replaced, did, err := detect(s)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if did {
							newJSON, err := jsonx.Marshal(replaced)
							if err != nil {
								return nil, errors.New("pii: request body could not be processed for anonymization")
							}
							promptArr[i] = jsonx.RawMessage(newJSON)
							arrTouched = true
						}
						continue
					}
					// Not a string: must be an integer or an array-of-integers (token IDs).
					// Validate the element so that objects, bools, floats, and other
					// unexpected types are rejected (fail-closed).
					if !isTokenElement(elem, 0) {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					// Valid token-ID element (integer or int[]): leave unchanged.
				}
				if arrTouched {
					newJSON, err := jsonx.Marshal(promptArr)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					doc["prompt"] = jsonx.RawMessage(newJSON)
					touched = true
				}
			} else {
				// "prompt" is present but is neither a string nor an array:
				// unsupported shape for a covered field → fail-closed.
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
		}
	}

	// ── embeddings "input" field ─────────────────────────────────────────────
	// /v1/embeddings carries text in the top-level "input" field, which may be
	// a string or an array of strings (or array of token-integer-arrays, which
	// are left unchanged). When "input" is present but is neither a string nor
	// an array, its shape is unsupported for the covered field — reject the
	// request (fail-closed) rather than forwarding unscanned content.
	if rawInput, ok := doc["input"]; ok {
		var inputStr string
		if err := jsonx.Unmarshal(rawInput, &inputStr); err == nil {
			// String input: scan and replace.
			replaced, did, err := detect(inputStr)
			if err != nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			if did {
				newJSON, err := jsonx.Marshal(replaced)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				doc["input"] = jsonx.RawMessage(newJSON)
				touched = true
			}
		} else {
			// Not a string: try array.
			var inputArr []jsonx.RawMessage
			if err2 := jsonx.Unmarshal(rawInput, &inputArr); err2 == nil {
				arrTouched := false
				for i, elem := range inputArr {
					// Each element may be a string or an integer array (token array,
					// int[][]). Scan string elements; leave integer and integer-array
					// elements untouched. Fail-closed on any other shape (object,
					// bool, null, float) — those are not valid OpenAI input element types.
					var s string
					if err := jsonx.Unmarshal(elem, &s); err == nil {
						replaced, did, err := detect(s)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if did {
							newJSON, err := jsonx.Marshal(replaced)
							if err != nil {
								return nil, errors.New("pii: request body could not be processed for anonymization")
							}
							inputArr[i] = jsonx.RawMessage(newJSON)
							arrTouched = true
						}
						continue
					}
					// Not a string: must be an integer or array-of-integers (token IDs).
					// Validate the element so that objects, bools, floats, and other
					// unexpected types are rejected (fail-closed).
					if !isTokenElement(elem, 0) {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					// Valid token-ID element: leave unchanged.
				}
				if arrTouched {
					newJSON, err := jsonx.Marshal(inputArr)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					doc["input"] = jsonx.RawMessage(newJSON)
					touched = true
				}
			} else {
				// "input" is present but is neither a string nor an array:
				// unsupported shape for a covered field → fail-closed.
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
		}
	}

	// ── tools[].function.description + parameters string leaves ─────────────
	// tools[].function.description is scanned for PII.
	// tools[].function.parameters: only string LEAF values are scanned (e.g.
	// description, default, enum strings, title). Object keys and structure are
	// never modified. "tools" present but not an array → fail-closed.
	if rawTools, ok := doc["tools"]; ok {
		var tools []jsonx.RawMessage
		if err := jsonx.Unmarshal(rawTools, &tools); err != nil || tools == nil {
			// "tools" is present but not an array (or is JSON null): unsupported shape.
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}
		toolsTouched := false
		for i, rawTool := range tools {
			var tool map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawTool, &tool); err != nil || tool == nil {
				// tools[] element is not a JSON object (or is JSON null): unsupported shape → fail-closed.
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			rawFn, hasFn := tool["function"]
			if !hasFn {
				continue
			}
			var fn map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawFn, &fn); err != nil || fn == nil {
				// tools[].function is not a JSON object (or is JSON null): unsupported shape → fail-closed.
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			fnTouched := false

			// tools[].function.description
			// Fail-closed: when "description" is present but is not a string,
			// reject rather than silently forwarding unscanned content.
			if rawDesc, hasDesc := fn["description"]; hasDesc {
				var desc string
				if err := jsonx.Unmarshal(rawDesc, &desc); err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				replaced, did, err := detect(desc)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if did {
					newJSON, err := jsonx.Marshal(replaced)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					fn["description"] = jsonx.RawMessage(newJSON)
					fnTouched = true
				}
			}

			// tools[].function.parameters: scan string leaf values only.
			if rawParams, hasParams := fn["parameters"]; hasParams {
				scanned, paramsTouched, err := scanStringLeaves(rawParams, detect)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if paramsTouched {
					fn["parameters"] = scanned
					fnTouched = true
				}
			}

			if fnTouched {
				newFnJSON, err := jsonx.Marshal(fn)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				tool["function"] = jsonx.RawMessage(newFnJSON)
				newToolJSON, err := jsonx.Marshal(tool)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				tools[i] = jsonx.RawMessage(newToolJSON)
				toolsTouched = true
			}
		}
		if toolsTouched {
			newToolsJSON, err := jsonx.Marshal(tools)
			if err != nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			doc["tools"] = jsonx.RawMessage(newToolsJSON)
			touched = true
		}
	}

	// ── messages array ───────────────────────────────────────────────────────
	rawMessages, hasMessages := doc["messages"]
	if hasMessages {
		var messages []jsonx.RawMessage
		if err := jsonx.Unmarshal(rawMessages, &messages); err != nil || messages == nil {
			// "messages" is present but not an array (or is JSON null): unsupported shape.
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}

		for i, rawMsg := range messages {
			var msg map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawMsg, &msg); err != nil || msg == nil {
				// messages[] element is not a JSON object (or is JSON null): unsupported shape → fail-closed.
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			msgTouched := false

			// messages[].name
			// Fail-closed: when "name" is present but is not a string, reject
			// rather than silently forwarding unscanned content.
			if rawName, ok := msg["name"]; ok {
				var name string
				if err := jsonx.Unmarshal(rawName, &name); err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				replaced, did, err := detect(name)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if did {
					newJSON, err := jsonx.Marshal(replaced)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					msg["name"] = jsonx.RawMessage(newJSON)
					msgTouched = true
				}
			}

			// messages[].content (string or array-of-parts).
			// Fail-closed: when "content" is present but is neither a string
			// nor an array, the shape is unsupported for a covered field — reject.
			if rawContent, ok := msg["content"]; ok {
				// JSON null is the legitimate "no text content" shape used by
				// assistant messages that carry only tool_calls (OpenAI spec).
				// There is nothing to scan; tool_calls on the same message are
				// handled separately below. Skip content scanning entirely —
				// do NOT set msgTouched, do NOT 422.
				if !bytes.Equal(bytes.TrimSpace([]byte(rawContent)), []byte("null")) {
					var strContent string
					if err := jsonx.Unmarshal(rawContent, &strContent); err == nil {
						// String content path.
						replaced, did, err := detect(strContent)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						if did {
							newJSON, err := jsonx.Marshal(replaced)
							if err != nil {
								return nil, errors.New("pii: request body could not be processed for anonymization")
							}
							msg["content"] = jsonx.RawMessage(newJSON)
							msgTouched = true
						}
					} else {
						// Array content (multi-modal parts) path.
						var parts []jsonx.RawMessage
						if err := jsonx.Unmarshal(rawContent, &parts); err == nil && parts != nil {
							partsTouched := false
							for j, rawPart := range parts {
								var part map[string]jsonx.RawMessage
								if err := jsonx.Unmarshal(rawPart, &part); err != nil || part == nil {
									// content array element is not a JSON object (or is JSON null) → fail-closed.
									return nil, errors.New("pii: request body could not be processed for anonymization")
								}
								rawType, hasType := part["type"]
								if !hasType {
									// content array element has no "type" field → fail-closed:
									// we cannot determine whether it carries text that needs scanning.
									return nil, errors.New("pii: request body could not be processed for anonymization")
								}
								var partType string
								if err := jsonx.Unmarshal(rawType, &partType); err != nil {
									// "type" field is not a string → fail-closed.
									return nil, errors.New("pii: request body could not be processed for anonymization")
								}
								if partType == "text" {
									// Text part: scan and replace PII in the "text" field.
									rawText, hasText := part["text"]
									if !hasText {
										// text part without a "text" field — nothing to scan; skip.
										continue
									}
									var textVal string
									if err := jsonx.Unmarshal(rawText, &textVal); err != nil {
										// "text" field is not a string → fail-closed.
										return nil, errors.New("pii: request body could not be processed for anonymization")
									}
									replaced, did, err := detect(textVal)
									if err != nil {
										return nil, errors.New("pii: request body could not be processed for anonymization")
									}
									if did {
										newJSON, err := jsonx.Marshal(replaced)
										if err != nil {
											return nil, errors.New("pii: request body could not be processed for anonymization")
										}
										part["text"] = jsonx.RawMessage(newJSON)
										newPartJSON, err := jsonx.Marshal(part)
										if err != nil {
											return nil, errors.New("pii: request body could not be processed for anonymization")
										}
										parts[j] = jsonx.RawMessage(newPartJSON)
										partsTouched = true
									}
								} else if knownContentPartTypes[partType] {
									// Known non-text part (image_url, input_audio, file, etc.):
									// carries no scannable text, pass through unchanged.
									continue
								} else {
									// Unknown type: we cannot determine whether this part carries
									// text that needs scanning → fail-closed (conservative).
									return nil, errors.New("pii: request body could not be processed for anonymization")
								}
							}
							if partsTouched {
								newPartsJSON, err := jsonx.Marshal(parts)
								if err != nil {
									return nil, errors.New("pii: request body could not be processed for anonymization")
								}
								msg["content"] = jsonx.RawMessage(newPartsJSON)
								msgTouched = true
							}
						} else {
							// "content" is present but is neither a string nor an array:
							// unsupported shape for a covered field → fail-closed.
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
					}
				}
			}

			// messages[].function_call.arguments (legacy function call).
			// Fail-closed: when "function_call" is present but not an object, or
			// "arguments" is present but not a string → reject.
			if rawFC, ok := msg["function_call"]; ok {
				var fc map[string]jsonx.RawMessage
				if err := jsonx.Unmarshal(rawFC, &fc); err != nil || fc == nil {
					// "function_call" present but not an object (or is JSON null): unsupported shape.
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				if rawArgs, ok := fc["arguments"]; ok {
					var args string
					if err := jsonx.Unmarshal(rawArgs, &args); err != nil {
						// "arguments" present but not a string: unsupported shape.
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					replaced, did, err := detect(args)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					if did {
						newJSON, err := jsonx.Marshal(replaced)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						fc["arguments"] = jsonx.RawMessage(newJSON)
						newFCJSON, err := jsonx.Marshal(fc)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						msg["function_call"] = jsonx.RawMessage(newFCJSON)
						msgTouched = true
					}
				}
			}

			// messages[].tool_calls[].function.arguments.
			// Fail-closed: when "tool_calls" is present but not an array → reject.
			// When an element is not an object, or "arguments" is not a string → reject.
			if rawTC, ok := msg["tool_calls"]; ok {
				var toolCalls []jsonx.RawMessage
				if err := jsonx.Unmarshal(rawTC, &toolCalls); err != nil || toolCalls == nil {
					// "tool_calls" present but not an array (or is JSON null): unsupported shape.
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				tcTouched := false
				for ti, rawCall := range toolCalls {
					var call map[string]jsonx.RawMessage
					if err := jsonx.Unmarshal(rawCall, &call); err != nil || call == nil {
						// tool_calls[] element is not a JSON object (or is JSON null) → fail-closed.
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					rawCallFn, hasFn := call["function"]
					if !hasFn {
						continue
					}
					var callFn map[string]jsonx.RawMessage
					if err := jsonx.Unmarshal(rawCallFn, &callFn); err != nil || callFn == nil {
						// tool_calls[].function is not a JSON object (or is JSON null) → fail-closed.
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					rawArgs, hasArgs := callFn["arguments"]
					if !hasArgs {
						continue
					}
					var args string
					if err := jsonx.Unmarshal(rawArgs, &args); err != nil {
						// "arguments" present but not a string: unsupported shape.
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					replaced, did, err := detect(args)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					if did {
						newJSON, err := jsonx.Marshal(replaced)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						callFn["arguments"] = jsonx.RawMessage(newJSON)
						newCallFnJSON, err := jsonx.Marshal(callFn)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						call["function"] = jsonx.RawMessage(newCallFnJSON)
						newCallJSON, err := jsonx.Marshal(call)
						if err != nil {
							return nil, errors.New("pii: request body could not be processed for anonymization")
						}
						toolCalls[ti] = jsonx.RawMessage(newCallJSON)
						tcTouched = true
					}
				}
				if tcTouched {
					newTCJSON, err := jsonx.Marshal(toolCalls)
					if err != nil {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					msg["tool_calls"] = jsonx.RawMessage(newTCJSON)
					msgTouched = true
				}
			}

			if msgTouched {
				newMsgJSON, err := jsonx.Marshal(msg)
				if err != nil {
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				messages[i] = jsonx.RawMessage(newMsgJSON)
				touched = true
			}
		}

		if touched {
			newMessagesJSON, err := jsonx.Marshal(messages)
			if err != nil {
				return nil, errors.New("pii: request body could not be processed for anonymization")
			}
			doc["messages"] = jsonx.RawMessage(newMessagesJSON)
		}
	}

	// ── every other top-level field: scanned by default ─────────────────────
	// Any top-level key that is neither explicitly covered above nor in the
	// structural exempt set is potential free-text content. Scan it with
	// scanStringLeaves: every string leaf is pseudonymized independently
	// (never concatenated), object keys are checked for PII and rejected
	// fail-closed, recursion is bounded by maxScanDepth, and numbers, booleans,
	// and null are left untouched. This is what covers rerank/score fields
	// (query, documents, texts, text_1, text_2, ...) and any other current or
	// future field without endpoint-specific code.
	//
	// Keys are collected and sorted before iterating, rather than ranged over
	// directly, so that processing order is deterministic across runs — Go's
	// map iteration order is randomized, and pseudonymFor records the
	// first-seen spelling of a case-normalized value (e.g. "User@Example.com"
	// vs. "user@example.com") for Restore. Without a fixed order, which
	// spelling wins would vary run to run.
	unknownKeys := make([]string, 0, len(doc))
	for key := range doc {
		if coveredTopLevelFields[key] || exemptTopLevelFields[key] {
			continue
		}
		unknownKeys = append(unknownKeys, key)
	}
	sort.Strings(unknownKeys)
	for _, key := range unknownKeys {
		raw := doc[key]
		scanned, did, err := scanStringLeaves(raw, detect)
		if err != nil {
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}
		if did {
			doc[key] = scanned
			touched = true
		}
	}

	if !touched {
		// No modifications: return a fresh copy of the original (not the
		// re-serialized doc) to preserve byte-for-byte fidelity and avoid
		// a pointless round-trip.
		out := make([]byte, len(body))
		copy(out, body)
		return out, nil
	}

	out, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, errors.New("pii: request body could not be processed for anonymization")
	}
	return out, nil
}

// replaceSpansInText substitutes the given non-overlapping, Start-sorted
// spans in text with pseudonyms returned by replace. A single left-to-right
// pass over the sorted spans builds the result with a strings.Builder,
// copying the unchanged gap between consecutive spans directly. This is O(n)
// in the length of text with at most one allocation for the Builder's buffer.
// Returns an error if replace returns an error for any span.
func replaceSpansInText(text string, spans []Span, replace func(typ, value string) (string, error)) (string, bool, error) {
	if len(spans) == 0 {
		return text, false, nil
	}
	var b strings.Builder
	b.Grow(len(text)) // pre-size: result length is close to input length
	cursor := 0
	for _, s := range spans {
		// Copy the unchanged prefix between the previous span's end and this
		// span's start. Spans are Start-sorted and non-overlapping, so cursor
		// is always <= s.Start.
		b.WriteString(text[cursor:s.Start])
		orig := text[s.Start:s.End]
		pseudo, err := replace(s.Type, orig)
		if err != nil {
			return "", false, err
		}
		b.WriteString(pseudo)
		cursor = s.End
	}
	// Append any trailing text after the last span.
	b.WriteString(text[cursor:])
	return b.String(), true, nil
}

// deOverlap merges overlapping spans from a Start-ascending sorted slice
// into their union intervals, guaranteeing that no byte flagged by any
// detector is left unmasked.
//
// Algorithm: maintain an accumulated interval [curStart, curEnd) and the
// "dominant" span for that interval (the one whose matched text covers the
// most bytes; ties broken by the span seen first, i.e. leftmost). For each
// subsequent span, if it overlaps or is adjacent to the accumulated interval,
// extend the interval to max(curEnd, span.End) and update the dominant type
// to the longest contributing span. When a span is strictly disjoint, flush
// the accumulated interval as a single Span and start a new one.
//
// The Type of a merged span is taken from the longest contributing span
// (largest End−Start); on a tie the first-seen (leftmost) span's type is
// kept. The original text covered by the union is text[curStart:curEnd],
// which is passed as a single value to replace() so that pseudonymization
// treats the union as one PII entity and Restore always round-trips the
// exact union substring.
//
// Privacy invariant: every byte in any input Span appears in exactly one
// output Span — no byte is dropped. The merged output may over-mask the
// gap bytes between two overlapping spans, which is acceptable: the
// worst case is slight over-anonymization, never under-anonymization.
func deOverlap(spans []Span) []Span {
	if len(spans) == 0 {
		return spans
	}
	result := make([]Span, 0, len(spans))

	cur := spans[0]
	curLen := cur.End - cur.Start

	for _, s := range spans[1:] {
		if s.Start < cur.End {
			// Overlapping or contained: merge into union.
			// (s.Start == cur.End would be adjacent but is intentionally left
			// disjoint; the condition is strictly less-than, not less-or-equal.)
			if s.End > cur.End {
				cur.End = s.End
			}
			// Prefer the type of the longest contributing span; ties keep cur.
			sLen := s.End - s.Start
			if sLen > curLen {
				cur.Type = s.Type
				curLen = sLen
			}
			continue
		}
		// Disjoint: flush the accumulated span.
		result = append(result, cur)
		cur = s
		curLen = cur.End - cur.Start
	}
	result = append(result, cur)
	return result
}

// toolChoiceFunctionNamePattern is the charset OpenAI restricts function
// names to: 1-64 characters drawn from letters, digits, underscore, and
// hyphen. tool_choice.function.name is validated against this pattern and,
// when it matches, left untouched rather than scanned — the name is a
// structural identifier the upstream must receive byte-identical to route
// the tool call correctly.
var toolChoiceFunctionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// maxLogitBiasKeyDigits is the maximum length of a logit_bias object key
// that isLogitBiasTokenIDKey accepts as a token ID. Tokenizer vocabularies
// are well below 10 million entries (7 digits), so a legitimate token ID
// never needs more than maxLogitBiasKeyDigits digits. Longer digit strings
// could carry a card number, phone number, or similar unscanned free-text
// value smuggled through a structurally-exempt field, so they are rejected
// fail-closed rather than treated as a token ID.
const maxLogitBiasKeyDigits = 7

// isLogitBiasTokenIDKey reports whether s is a valid logit_bias object key:
// 1 to maxLogitBiasKeyDigits ASCII digit characters ('0'-'9'), with no other
// characters allowed. It is used to validate logit_bias object keys, which
// must be token IDs (numeric strings within the tokenizer's vocabulary
// range) for the field to be exempt from scanning.
func isLogitBiasTokenIDKey(s string) bool {
	if len(s) == 0 || len(s) > maxLogitBiasKeyDigits {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isJSONNumberLiteral reports whether raw is a JSON number literal — not a
// string, boolean, null, object, or array. It is used to validate
// logit_bias object values, which must be bias numbers for the field to be
// exempt from scanning.
func isJSONNumberLiteral(raw jsonx.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '"', '{', '[', 't', 'f', 'n':
		return false
	}
	var n float64
	return jsonx.Unmarshal(trimmed, &n) == nil
}

// isTokenElement reports whether raw is a valid OpenAI token-ID element: either
// a JSON integer (single token ID) or a JSON array whose every element is itself
// a valid token-ID element (int[][]). Floats, objects, booleans, null, and
// strings are not token IDs and cause the function to return false.
//
// depth limits recursion to prevent a stack-exhaustion DoS via pathologically
// nested arrays. When depth > maxScanDepth the function returns false, which
// causes the caller to fail-closed on that element.
//
// This is used to validate non-string elements in the "prompt" and "input"
// array fields before passing them through unscanned, ensuring that unexpected
// shapes are rejected (fail-closed) rather than silently forwarded.
func isTokenElement(raw jsonx.RawMessage, depth int) bool {
	if depth > maxScanDepth {
		// Reject pathologically deep arrays to prevent stack exhaustion.
		return false
	}

	// A JSON integer: valid single token ID. We require an exact integer
	// (int64 unmarshal succeeds without loss). Floats/decimals are rejected
	// because token IDs are always whole numbers; accepting floats would
	// allow unexpected numeric shapes to pass through unscanned.
	//
	// Strategy: unmarshal as int64. If that succeeds, it is an integer.
	// If the raw bytes contain a decimal point or exponent indicating a
	// non-integer float, int64 unmarshal will fail, so we check the raw
	// bytes for those markers before concluding it is not an integer.
	var n int64
	if jsonx.Unmarshal(raw, &n) == nil {
		// Verify raw bytes do not contain a decimal point or exponent —
		// some JSON libraries round floats to integers on unmarshal.
		rawStr := string(raw)
		for _, ch := range rawStr {
			if ch == '.' || ch == 'e' || ch == 'E' {
				return false // float-shaped literal, not a token ID
			}
		}
		return true
	}

	// An array: every element must itself be a valid token element (int[]).
	var arr []jsonx.RawMessage
	if jsonx.Unmarshal(raw, &arr) == nil {
		for _, elem := range arr {
			if !isTokenElement(elem, depth+1) {
				return false
			}
		}
		return true
	}

	// Anything else (object, bool, null, string, float) is not a token ID.
	return false
}

// maxScanDepth is the maximum recursion depth for scanStringLeavesDepth and
// for isTokenElement. A tools[].function.parameters JSON Schema is unlikely to
// exceed a handful of nesting levels; 128 is a generous upper bound that still
// prevents a malicious or pathologically nested document from causing a
// goroutine stack overflow.
const maxScanDepth = 128

// scanStringLeaves recursively traverses a JSON value encoded as RawMessage
// and applies detect to every string leaf it finds. Object keys are never
// modified; only string values are scanned. Arrays of non-strings are
// traversed recursively but non-string leaves are left untouched.
//
// This is used to scan tools[].function.parameters, which is a JSON Schema
// object that may contain PII in string-valued fields (description, default,
// enum strings, title, etc.) while its structure (object shape, key names)
// must be preserved exactly.
//
// Recursion is bounded by maxScanDepth. A body whose parameters object is
// nested beyond that limit is rejected (fail-closed) rather than traversed.
//
// Returns the (possibly modified) RawMessage, a bool indicating whether any
// replacement was made, and any error from detect or re-serialization.
func scanStringLeaves(raw jsonx.RawMessage, detect func(string) (string, bool, error)) (jsonx.RawMessage, bool, error) {
	return scanStringLeavesDepth(raw, detect, 0)
}

// scanStringLeavesDepth is the depth-bounded implementation of scanStringLeaves.
// depth is the current recursion depth; the initial caller passes 0.
func scanStringLeavesDepth(raw jsonx.RawMessage, detect func(string) (string, bool, error), depth int) (jsonx.RawMessage, bool, error) {
	if depth > maxScanDepth {
		return nil, false, errors.New("pii: parameters schema exceeds maximum nesting depth")
	}

	// Try string first.
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		replaced, did, err := detect(s)
		if err != nil {
			return nil, false, err
		}
		if !did {
			return raw, false, nil
		}
		newJSON, err := jsonx.Marshal(replaced)
		if err != nil {
			return nil, false, err
		}
		return jsonx.RawMessage(newJSON), true, nil
	}

	// Try object: scan each key and each value recursively.
	//
	// Key scanning: object keys in a JSON Schema (tools[].function.parameters)
	// are structural identifiers. Pseudonymizing a key would corrupt the schema
	// (the upstream model expects the original field names). Therefore, if any
	// key matches a PII pattern we fail-closed rather than forwarding unscanned
	// or corrupted content.
	//
	// Keys are collected and sorted before iterating, rather than ranged over
	// directly, so that nested traversal order is deterministic across runs —
	// see the identical rationale on the top-level unknown-field scan loop in
	// anonymizeWithDetectors.
	var obj map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &obj); err == nil {
		objTouched := false
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := obj[k]
			// Scan the key for PII. If the key contains PII, fail-closed:
			// rewriting a structural key would corrupt the schema.
			_, keyHasPII, err := detect(k)
			if err != nil {
				return nil, false, err
			}
			if keyHasPII {
				return nil, false, errors.New("pii: parameters schema contains PII in an object key")
			}
			scanned, did, err := scanStringLeavesDepth(v, detect, depth+1)
			if err != nil {
				return nil, false, err
			}
			if did {
				obj[k] = scanned
				objTouched = true
			}
		}
		if !objTouched {
			return raw, false, nil
		}
		newJSON, err := jsonx.Marshal(obj)
		if err != nil {
			return nil, false, err
		}
		return jsonx.RawMessage(newJSON), true, nil
	}

	// Try array: scan each element recursively.
	var arr []jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &arr); err == nil {
		arrTouched := false
		for i, elem := range arr {
			scanned, did, err := scanStringLeavesDepth(elem, detect, depth+1)
			if err != nil {
				return nil, false, err
			}
			if did {
				arr[i] = scanned
				arrTouched = true
			}
		}
		if !arrTouched {
			return raw, false, nil
		}
		newJSON, err := jsonx.Marshal(arr)
		if err != nil {
			return nil, false, err
		}
		return jsonx.RawMessage(newJSON), true, nil
	}

	// Scalar (number, bool, null): leave unchanged.
	return raw, false, nil
}
