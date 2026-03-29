// Package jsonx provides JSON encoding and decoding backed by sonic (SIMD-accelerated).
// It re-exports sonic's functions under the same names as encoding/json and adds
// RawMessage as a type alias so callers need only one import.
package jsonx

import (
	stdjson "encoding/json"
	"io"

	"github.com/bytedance/sonic"
)

// ConfigStd matches encoding/json behavior: HTML escaping, sorted map keys,
// and UTF-8 validation enabled. This is the safest default for a proxy that
// forwards user-controlled content.
var cfg = sonic.ConfigStd

// Marshal returns the JSON encoding of v.
var Marshal = cfg.Marshal

// Unmarshal parses the JSON-encoded data and stores the result in v.
var Unmarshal = cfg.Unmarshal

// MarshalIndent is like Marshal but applies Indent to format the output.
var MarshalIndent = cfg.MarshalIndent

// Valid reports whether data is a valid JSON encoding.
var Valid = sonic.Valid // Valid is stateless, no config needed

// RawMessage is a raw encoded JSON value. It is compatible with encoding/json.RawMessage.
type RawMessage = stdjson.RawMessage

// NewEncoder returns a new streaming JSON encoder that writes to w.
// Falls back to stdlib for streaming — sonic's Encoder has a different interface.
// Streaming is only used in cold-path locations (license heartbeat).
func NewEncoder(w io.Writer) *stdjson.Encoder {
	return stdjson.NewEncoder(w)
}

// NewDecoder returns a new streaming JSON decoder that reads from r.
// Falls back to stdlib for streaming — sonic's Decoder has a different interface.
func NewDecoder(r io.Reader) *stdjson.Decoder {
	return stdjson.NewDecoder(r)
}
