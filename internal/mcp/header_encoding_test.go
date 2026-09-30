package mcp_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestEncodeHeaderValue covers the base64-sentinel encoding table from MCP
// Streamable HTTP §4.4 (docs/mcp-v2.md §4.4): visible ASCII passes through
// unchanged, everything else — non-ASCII, leading/trailing whitespace, and a
// value that already looks like the sentinel wrapper itself — must be
// base64-encoded.
func TestEncodeHeaderValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "pure ASCII is unchanged",
			in:   "us-west1",
			want: "us-west1",
		},
		{
			name: "empty string is unchanged",
			in:   "",
			want: "",
		},
		{
			name: "non-ASCII is base64-sentinel-encoded",
			in:   "Hello, 世界",
			want: "=?base64?SGVsbG8sIOS4lueVjA==?=",
		},
		{
			name: "leading space forces encoding",
			in:   " padded",
			want: "=?base64?IHBhZGRlZA==?=",
		},
		{
			name: "trailing space forces encoding",
			in:   "padded ",
			want: "=?base64?cGFkZGVkIA==?=",
		},
		{
			name: "leading and trailing spaces force encoding",
			in:   " padded ",
			want: "=?base64?IHBhZGRlZCA=?=",
		},
		{
			name: "a value that already looks like the sentinel is itself encoded",
			in:   "=?base64?dGVzdA==?=",
			want: "=?base64?PT9iYXNlNjQ/ZEdWemRBPT0/PQ==?=",
		},
		{
			name: "control characters force encoding",
			in:   "a\tb",
			want: "=?base64?YQli?=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.EncodeHeaderValue(tc.in)
			if got != tc.want {
				t.Errorf("EncodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDecodeHeaderValue_SentinelCaseSensitivity verifies the prefix/suffix
// are matched case-sensitively: a value that merely LOOKS like the sentinel
// under a different case must NOT be treated as one — it passes through
// unchanged, exactly as the spec requires ("=?base64?" and "?=" are
// case-sensitive and must appear exactly as written on the wire").
func TestDecodeHeaderValue_SentinelCaseSensitivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          string
		wantDecoded string
		wantOK      bool
	}{
		{
			name:        "uppercase BASE64 prefix is not the sentinel — passed through unchanged",
			in:          "=?BASE64?dGVzdA==?=",
			wantDecoded: "=?BASE64?dGVzdA==?=",
			wantOK:      true,
		},
		{
			name:        "mixed-case Base64 prefix is not the sentinel — passed through unchanged",
			in:          "=?Base64?dGVzdA==?=",
			wantDecoded: "=?Base64?dGVzdA==?=",
			wantOK:      true,
		},
		{
			name:        "exact lowercase sentinel is decoded",
			in:          "=?base64?dGVzdA==?=",
			wantDecoded: "test",
			wantOK:      true,
		},
		{
			name:        "suffix must be exactly ?= — a near-miss is not a sentinel",
			in:          "=?base64?dGVzdA===",
			wantDecoded: "=?base64?dGVzdA===",
			wantOK:      true,
		},
		{
			name:   "sentinel-shaped but invalid base64 payload reports ok=false",
			in:     "=?base64?not valid base64!!?=",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := mcp.DecodeHeaderValue(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("DecodeHeaderValue(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if tc.wantOK && got != tc.wantDecoded {
				t.Errorf("DecodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.wantDecoded)
			}
		})
	}
}

// TestHeaderValue_EncodeDecodeRoundTrip verifies EncodeHeaderValue followed
// by DecodeHeaderValue always reproduces the original value, across ASCII,
// non-ASCII, whitespace-padded, sentinel-shaped, and empty inputs.
func TestHeaderValue_EncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	values := []string{
		"",
		"us-west1",
		"tools/call",
		"get_weather",
		"Hello, 世界",
		" padded ",
		"padded ",
		" padded",
		"=?base64?dGVzdA==?=",   // already sentinel-shaped
		"=?BASE64?not-really?=", // sentinel-shaped but wrong case — still round-trips as plain ASCII
		"a\nb\tc",
		"emoji: 🎉🚀",
	}

	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			t.Parallel()

			encoded := mcp.EncodeHeaderValue(v)
			decoded, ok := mcp.DecodeHeaderValue(encoded)
			if !ok {
				t.Fatalf("DecodeHeaderValue(%q) ok = false, want true", encoded)
			}
			if decoded != v {
				t.Errorf("round trip: EncodeHeaderValue(%q) = %q, DecodeHeaderValue(...) = %q, want %q", v, encoded, decoded, v)
			}
		})
	}
}

// TestDecodeHeaderValue_NonSentinelPassesThrough verifies a value carrying
// no sentinel wrapper at all is returned unchanged with ok=true — the common
// case for the vast majority of header values.
func TestDecodeHeaderValue_NonSentinelPassesThrough(t *testing.T) {
	t.Parallel()

	got, ok := mcp.DecodeHeaderValue("plain-ascii-value")
	if !ok {
		t.Fatal("DecodeHeaderValue() ok = false, want true")
	}
	if got != "plain-ascii-value" {
		t.Errorf("DecodeHeaderValue() = %q, want %q", got, "plain-ascii-value")
	}
}

// ---- ValidParamHeaderName / ValidParamHeaderValue --------------------------

// TestValidParamHeaderName_Table covers every rule ValidParamHeaderName
// applies to a Mcp-Param-{Name} header NAME (MCP 2026-07-28 §4.3): the
// HeaderParamPrefix match is case-insensitive, the suffix must be non-empty,
// the suffix must itself be a valid RFC 9110 §5.1 HTTP token, and the suffix
// is capped at MaxParamHeaderNameLength bytes. Rot the moment any one of
// those checks is loosened or the boundary shifts by one byte in either
// direction.
func TestValidParamHeaderName_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "ordinary valid name", header: "Mcp-Param-Region", want: true},
		{name: "prefix match is case-insensitive", header: "mcp-param-Region", want: true},
		{name: "uppercase prefix and suffix", header: "MCP-PARAM-REGION", want: true},
		{name: "exact prefix with empty suffix is rejected", header: "Mcp-Param-", want: false},
		{name: "missing the prefix entirely", header: "X-Param-Region", want: false},
		{name: "prefix present but header shorter than the full prefix", header: "Mcp-Param", want: false},
		{name: "space in the suffix is not a valid HTTP token", header: "Mcp-Param-Region Two", want: false},
		{name: "@ in the suffix is not a valid HTTP token", header: "Mcp-Param-Reg@ion", want: false},
		{name: "suffix at exactly MaxParamHeaderNameLength (64) is accepted", header: "Mcp-Param-" + strings.Repeat("a", mcp.MaxParamHeaderNameLength), want: true},
		{name: "suffix one byte over MaxParamHeaderNameLength is rejected", header: "Mcp-Param-" + strings.Repeat("a", mcp.MaxParamHeaderNameLength+1), want: false},
		{name: "empty string", header: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := mcp.ValidParamHeaderName(tc.header); got != tc.want {
				t.Errorf("ValidParamHeaderName(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

// TestValidParamHeaderValue_Table covers ValidParamHeaderValue's rules:
// non-empty, at most MaxParamHeaderValueLength bytes, and visible ASCII with
// no leading/trailing whitespace.
//
// The control-character cases (CR, LF, NUL) here are deliberately exercised
// as DIRECT Go-string inputs to the function, not through any HTTP transport
// round trip: net/http's own header writer (net/http/header.go,
// headerNewlineToSpace) replaces \r and \n with spaces before a request ever
// reaches the wire in either direction (inbound via httptest, or outbound via
// HTTPTransport's rawPost/Forward), and fasthttp's RequestHeader.Set strips
// newlines from a value on the way in too (valyala/fasthttp's
// removeNewLines) — so a literal CR or LF byte inside a header VALUE can
// never actually survive a real request/response cycle to reach
// collectMCPParamHeaders in production. ValidParamHeaderValue's control-
// character rejection is still exercised directly here because it is a
// documented part of the function's own contract (shared with
// EncodeHeaderValue's isVisibleASCIIHeaderValue) and a legitimate defense in
// depth against any future caller that does not go through net/http or
// fasthttp to construct hdr.
func TestValidParamHeaderValue_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "ordinary ASCII value", value: "us-west1", want: true},
		{name: "empty value is rejected (unlike isVisibleASCIIHeaderValue alone)", value: "", want: false},
		{name: "leading space is rejected", value: " us-west1", want: false},
		{name: "trailing space is rejected", value: "us-west1 ", want: false},
		{name: "embedded carriage return is rejected", value: "us-west1\rEvil", want: false},
		{name: "embedded line feed is rejected", value: "us-west1\nEvil", want: false},
		{name: "embedded NUL byte is rejected", value: "us-west1\x00Evil", want: false},
		{name: "a non-ASCII byte is rejected (must be pre-encoded via EncodeHeaderValue)", value: "café", want: false},
		{name: "value at exactly MaxParamHeaderValueLength (1024) is accepted", value: strings.Repeat("a", mcp.MaxParamHeaderValueLength), want: true},
		{name: "value one byte over MaxParamHeaderValueLength is rejected", value: strings.Repeat("a", mcp.MaxParamHeaderValueLength+1), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := mcp.ValidParamHeaderValue(tc.value); got != tc.want {
				t.Errorf("ValidParamHeaderValue(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
