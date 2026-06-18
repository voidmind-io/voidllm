package pii

import (
	"errors"
	"sort"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// anonymizeWithDetectors replaces PII in all PII-bearing string fields of an
// OpenAI-shaped request body. It handles chat completion, legacy completion,
// and embeddings request shapes. Covered fields:
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
			spans = append(spans, d.Find(text)...)
		}
		if len(spans) == 0 {
			return text, false, nil
		}
		// Sort and de-overlap merged spans from all detectors.
		sort.Slice(spans, func(i, j int) bool {
			if spans[i].Start != spans[j].Start {
				return spans[i].Start < spans[j].Start
			}
			return spans[i].End > spans[j].End // longest first on tie
		})
		spans = deOverlap(spans)
		out, touched, err := replaceSpansInText(text, spans, replace)
		return out, touched, err
	}

	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
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
					// Not a string: must be a number or a number-array (token IDs).
					// Validate the element is an integer or an array-of-integers so
					// that objects, bools, and other unexpected types are rejected.
					if !isTokenElement(elem) {
						return nil, errors.New("pii: request body could not be processed for anonymization")
					}
					// Valid token-ID element (number or int[]): leave unchanged.
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
					// bool, null) — those are not valid OpenAI input element types.
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
					// Not a string: must be a number or a number-array (token IDs).
					// Validate the element so that objects, bools, and other
					// unexpected types are rejected (fail-closed).
					if !isTokenElement(elem) {
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
		if err := jsonx.Unmarshal(rawTools, &tools); err != nil {
			// "tools" is present but not an array: unsupported shape.
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}
		toolsTouched := false
		for i, rawTool := range tools {
			var tool map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawTool, &tool); err != nil {
				continue
			}
			rawFn, hasFn := tool["function"]
			if !hasFn {
				continue
			}
			var fn map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawFn, &fn); err != nil {
				continue
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
		if err := jsonx.Unmarshal(rawMessages, &messages); err != nil {
			return nil, errors.New("pii: request body could not be processed for anonymization")
		}

		for i, rawMsg := range messages {
			var msg map[string]jsonx.RawMessage
			if err := jsonx.Unmarshal(rawMsg, &msg); err != nil {
				continue
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
					if err := jsonx.Unmarshal(rawContent, &parts); err == nil {
						partsTouched := false
						for j, rawPart := range parts {
							var part map[string]jsonx.RawMessage
							if err := jsonx.Unmarshal(rawPart, &part); err != nil {
								continue
							}
							var partType string
							if rawType, hasType := part["type"]; hasType {
								_ = jsonx.Unmarshal(rawType, &partType)
							}
							if partType != "text" {
								continue
							}
							rawText, hasText := part["text"]
							if !hasText {
								continue
							}
							var textVal string
							if err := jsonx.Unmarshal(rawText, &textVal); err != nil {
								continue
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

			// messages[].function_call.arguments (legacy function call).
			// Fail-closed: when "function_call" is present but not an object, or
			// "arguments" is present but not a string → reject.
			if rawFC, ok := msg["function_call"]; ok {
				var fc map[string]jsonx.RawMessage
				if err := jsonx.Unmarshal(rawFC, &fc); err != nil {
					// "function_call" present but not an object: unsupported shape.
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
			// When "arguments" is present but not a string within a tool call → reject.
			if rawTC, ok := msg["tool_calls"]; ok {
				var toolCalls []jsonx.RawMessage
				if err := jsonx.Unmarshal(rawTC, &toolCalls); err != nil {
					// "tool_calls" present but not an array: unsupported shape.
					return nil, errors.New("pii: request body could not be processed for anonymization")
				}
				tcTouched := false
				for ti, rawCall := range toolCalls {
					var call map[string]jsonx.RawMessage
					if err := jsonx.Unmarshal(rawCall, &call); err != nil {
						continue
					}
					rawCallFn, hasFn := call["function"]
					if !hasFn {
						continue
					}
					var callFn map[string]jsonx.RawMessage
					if err := jsonx.Unmarshal(rawCallFn, &callFn); err != nil {
						continue
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

// deOverlap removes overlapping spans from a Start-ascending sorted slice,
// keeping the leftmost span and discarding any that starts before the
// previous span ends.
func deOverlap(spans []Span) []Span {
	if len(spans) == 0 {
		return spans
	}
	result := spans[:1]
	cursor := spans[0].End
	for _, s := range spans[1:] {
		if s.Start < cursor {
			continue
		}
		result = append(result, s)
		cursor = s.End
	}
	return result
}

// isTokenElement reports whether raw is a valid OpenAI token-ID element: either
// a JSON number (single token ID) or a JSON array whose every element is itself
// a valid token-ID element (int[][]). Objects, booleans, null, and strings are
// not token IDs and cause the function to return false. This is used to validate
// non-string elements in the "prompt" and "input" array fields before passing
// them through unscanned, ensuring that unexpected shapes are rejected
// (fail-closed) rather than silently forwarded.
func isTokenElement(raw jsonx.RawMessage) bool {
	// A number: valid single token ID. Try to unmarshal as int64 first (exact
	// for token IDs), then float64 (handles any numeric JSON literal).
	var n int64
	if jsonx.Unmarshal(raw, &n) == nil {
		return true
	}
	var f float64
	if jsonx.Unmarshal(raw, &f) == nil {
		return true
	}

	// An array: every element must itself be a valid token element (int[]).
	var arr []jsonx.RawMessage
	if jsonx.Unmarshal(raw, &arr) == nil {
		for _, elem := range arr {
			if !isTokenElement(elem) {
				return false
			}
		}
		return true
	}

	// Anything else (object, bool, null, string) is not a token ID.
	return false
}

// maxScanDepth is the maximum recursion depth for scanStringLeavesDepth.
// A tools[].function.parameters JSON Schema is unlikely to exceed a handful of
// nesting levels; 128 is a generous upper bound that still prevents a malicious
// or pathologically nested schema from causing a goroutine stack overflow.
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
	var obj map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &obj); err == nil {
		objTouched := false
		for k, v := range obj {
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
