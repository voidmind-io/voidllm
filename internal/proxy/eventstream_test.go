package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// encodeESFrame builds a valid event-stream frame from raw header bytes.
func encodeESFrame(headers, payload []byte) []byte {
	total := eventStreamPreludeSize + len(headers) + len(payload) + 4
	buf := make([]byte, 0, total)
	buf = binary.BigEndian.AppendUint32(buf, uint32(total))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(headers)))
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf[:8]))
	buf = append(buf, headers...)
	buf = append(buf, payload...)
	return binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
}

// esStringHeader encodes a string-typed header.
func esStringHeader(name, value string) []byte {
	b := []byte{byte(len(name))}
	b = append(b, name...)
	b = append(b, 7)
	b = binary.BigEndian.AppendUint16(b, uint16(len(value)))
	return append(b, value...)
}

func esEventFrame(eventType, payload string) []byte {
	h := append(esStringHeader(":message-type", "event"), esStringHeader(":event-type", eventType)...)
	return encodeESFrame(h, []byte(payload))
}

func TestEventStreamReader_MultipleFramesOneByteReader(t *testing.T) {
	t.Parallel()
	var in bytes.Buffer
	in.Write(esEventFrame("messageStart", `{"role":"assistant"}`))
	in.Write(esEventFrame("contentBlockDelta", "{\n \"delta\": {\"text\": \"hi\"}\n}"))

	r := newEventStreamReader(io.NopCloser(iotest.OneByteReader(&in)))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "data: {\"messageStart\":{\"role\":\"assistant\"}}\n\n" +
		"data: {\"contentBlockDelta\":{\"delta\":{\"text\":\"hi\"}}}\n\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEventStreamReader_ExceptionDropsPayload(t *testing.T) {
	t.Parallel()
	h := append(esStringHeader(":message-type", "exception"), esStringHeader(":exception-type", "throttlingException")...)
	frame := encodeESFrame(h, []byte(`{"message":"secret prompt text"}`))

	got, err := io.ReadAll(newEventStreamReader(io.NopCloser(bytes.NewReader(frame))))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "data: {\":exception\":\"throttlingException\"}\n\n" {
		t.Errorf("unexpected output %q", got)
	}
	if strings.Contains(string(got), "secret") {
		t.Error("payload leaked into output")
	}
}

func TestEventStreamReader_Errors(t *testing.T) {
	t.Parallel()
	valid := esEventFrame("messageStop", `{}`)

	badMsgCRC := bytes.Clone(valid)
	badMsgCRC[len(badMsgCRC)-1] ^= 0xff
	badPreludeCRC := bytes.Clone(valid)
	badPreludeCRC[9] ^= 0xff

	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"message crc", badMsgCRC, errEventStreamMessageCRC},
		{"prelude crc", badPreludeCRC, errEventStreamPreludeCRC},
		{"truncated prelude", valid[:5], errEventStreamTruncated},
		{"truncated frame", valid[:len(valid)-3], errEventStreamTruncated},
		{"non-json payload", esEventFrame("messageStop", `not json`), errEventStreamMalformed},
		{"bad event type", esEventFrame("bad type", `{}`), errEventStreamMalformed},
		{"missing message type", encodeESFrame(esStringHeader(":event-type", "x"), []byte(`{}`)), errEventStreamMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := io.ReadAll(newEventStreamReader(io.NopCloser(bytes.NewReader(tt.in))))
			if !errors.Is(err, tt.want) {
				t.Errorf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEventStreamReader_TooLargeRejectedBeforeRead(t *testing.T) {
	t.Parallel()
	var p []byte
	p = binary.BigEndian.AppendUint32(p, maxEventStreamMessageSize+1)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, crc32.ChecksumIEEE(p))

	_, err := io.ReadAll(newEventStreamReader(io.NopCloser(bytes.NewReader(p))))
	if !errors.Is(err, errEventStreamTooLarge) {
		t.Errorf("got %v, want too large", err)
	}
}

func TestEventStreamReader_CleanEOFAndSticky(t *testing.T) {
	t.Parallel()
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(nil)))
	buf := make([]byte, 8)
	for i := 0; i < 2; i++ {
		if _, err := r.Read(buf); !errors.Is(err, io.EOF) {
			t.Fatalf("read %d: got %v, want EOF", i, err)
		}
	}
}

func TestParseEventStreamHeaders_AllTypes(t *testing.T) {
	t.Parallel()
	var b []byte
	add := func(name string, typ byte, val []byte) {
		b = append(b, byte(len(name)))
		b = append(b, name...)
		b = append(b, typ)
		b = append(b, val...)
	}
	add("a", 0, nil)
	add("b", 1, nil)
	add("c", 2, make([]byte, 1))
	add("d", 3, make([]byte, 2))
	add("e", 4, make([]byte, 4))
	add("f", 5, make([]byte, 8))
	add("g", 6, []byte{0, 2, 'x', 'y'})
	add("h", 8, make([]byte, 8))
	add("i", 9, make([]byte, 16))
	b = append(b, esStringHeader(":message-type", "event")...)
	b = append(b, esStringHeader(":error-message", "secret")...)

	h, err := parseEventStreamHeaders(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if h.messageType != "event" {
		t.Errorf("messageType = %q", h.messageType)
	}

	for name, bad := range map[string][]byte{
		"unknown type": {1, 'a', 10},
		"zero name":    {0},
		"overrun":      {1, 'a', 7, 0, 9, 'x'},
		"duplicate":    append(esStringHeader(":event-type", "a"), esStringHeader(":event-type", "b")...),
	} {
		if _, err := parseEventStreamHeaders(bad); !errors.Is(err, errEventStreamMalformed) {
			t.Errorf("%s: got %v, want malformed", name, err)
		}
	}
}

func TestEventStreamReader_Close(t *testing.T) {
	t.Parallel()
	c := &esCloser{Reader: bytes.NewReader(nil)}
	if err := newEventStreamReader(c).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !c.closed {
		t.Error("source not closed")
	}
}

type esCloser struct {
	io.Reader
	closed bool
}

func (c *esCloser) Close() error { c.closed = true; return nil }

// esRawHeader encodes a header with an explicit value type and raw value bytes.
func esRawHeader(name string, typ byte, val []byte) []byte {
	b := []byte{byte(len(name))}
	b = append(b, name...)
	b = append(b, typ)
	return append(b, val...)
}

// esPrelude builds a prelude with a valid CRC for arbitrary lengths.
func esPrelude(total, headers uint32) []byte {
	p := binary.BigEndian.AppendUint32(nil, total)
	p = binary.BigEndian.AppendUint32(p, headers)
	return binary.BigEndian.AppendUint32(p, crc32.ChecksumIEEE(p))
}

// esReadAll decodes in fully and returns the output and terminal error.
func esReadAll(in []byte) (string, error) {
	got, err := io.ReadAll(newEventStreamReader(io.NopCloser(bytes.NewReader(in))))
	return string(got), err
}

func TestEventStreamReader_MultipleFramesHalfReader(t *testing.T) {
	t.Parallel()
	var in bytes.Buffer
	in.Write(esEventFrame("messageStart", `{"role":"assistant"}`))
	in.Write(esEventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hi"}}`))
	in.Write(esEventFrame("messageStop", `{"stopReason":"end_turn"}`))

	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"half":     iotest.HalfReader,
		"one byte": iotest.OneByteReader,
		"data err": iotest.DataErrReader,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newEventStreamReader(io.NopCloser(wrap(bytes.NewReader(in.Bytes()))))
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			want := "data: {\"messageStart\":{\"role\":\"assistant\"}}\n\n" +
				"data: {\"contentBlockDelta\":{\"contentBlockIndex\":0,\"delta\":{\"text\":\"hi\"}}}\n\n" +
				"data: {\"messageStop\":{\"stopReason\":\"end_turn\"}}\n\n"
			if string(got) != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestEventStreamReader_SmallReadBuffer(t *testing.T) {
	t.Parallel()
	frame := esEventFrame("messageStart", `{"role":"assistant"}`)
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(frame)))
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("Read: %v", err)
			}
			break
		}
	}
	if want := "data: {\"messageStart\":{\"role\":\"assistant\"}}\n\n"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEventStreamReader_ZeroLengthRead(t *testing.T) {
	t.Parallel()
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(nil)))
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Errorf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
}

func TestEventStreamReader_FrameErrors(t *testing.T) {
	t.Parallel()
	valid := esEventFrame("messageStop", `{}`)

	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"total below minimum", esPrelude(15, 0), errEventStreamMalformed},
		{"total zero", esPrelude(0, 0), errEventStreamMalformed},
		{"total above 1 MiB", esPrelude(maxEventStreamMessageSize+1, 0), errEventStreamTooLarge},
		{"total max uint32", esPrelude(0xffffffff, 0), errEventStreamTooLarge},
		{"headers above 128 KiB", esPrelude(maxEventStreamMessageSize, maxEventStreamHeadersSize+1), errEventStreamTooLarge},
		{"headers longer than frame", esPrelude(20, 10), errEventStreamMalformed},
		{"headers exactly total minus 16 plus one", esPrelude(32, 17), errEventStreamMalformed},
		{"truncated mid prelude", valid[:11], errEventStreamTruncated},
		{"truncated one byte", valid[:1], errEventStreamTruncated},
		{"truncated mid frame", valid[:len(valid)/2], errEventStreamTruncated},
		{"truncated after prelude", valid[:eventStreamPreludeSize], errEventStreamTruncated},
		{"unknown header type", encodeESFrame(esRawHeader("a", 10, nil), []byte(`{}`)), errEventStreamMalformed},
		{"header name length zero", encodeESFrame([]byte{0}, []byte(`{}`)), errEventStreamMalformed},
		{"string value overrun", encodeESFrame([]byte{1, 'a', 7, 0, 9, 'x'}, []byte(`{}`)), errEventStreamMalformed},
		{"name overrun", encodeESFrame([]byte{9, 'a'}, []byte(`{}`)), errEventStreamMalformed},
		{"missing value type", encodeESFrame([]byte{1, 'a'}, []byte(`{}`)), errEventStreamMalformed},
		{"fixed value overrun", encodeESFrame(esRawHeader("a", 5, []byte{1, 2}), []byte(`{}`)), errEventStreamMalformed},
		{"duplicate event-type", encodeESFrame(append(append(esStringHeader(":message-type", "event"),
			esStringHeader(":event-type", "a")...), esStringHeader(":event-type", "b")...), []byte(`{}`)), errEventStreamMalformed},
		{"duplicate message-type", encodeESFrame(append(esStringHeader(":message-type", "event"),
			esStringHeader(":message-type", "event")...), []byte(`{}`)), errEventStreamMalformed},
		{"missing message-type", encodeESFrame(esStringHeader(":event-type", "x"), []byte(`{}`)), errEventStreamMalformed},
		{"unknown message-type", encodeESFrame(esStringHeader(":message-type", "other"), []byte(`{}`)), errEventStreamMalformed},
		{"event-type with digits", esEventFrame("a1", `{}`), errEventStreamMalformed},
		{"event-type with quote", esEventFrame(`a"b`, `{}`), errEventStreamMalformed},
		{"event-type empty", esEventFrame("", `{}`), errEventStreamMalformed},
		{"event-type 65 chars", esEventFrame(strings.Repeat("a", 65), `{}`), errEventStreamMalformed},
		{"event without event-type", encodeESFrame(esStringHeader(":message-type", "event"), []byte(`{}`)), errEventStreamMalformed},
		{"empty payload", esEventFrame("x", ``), errEventStreamMalformed},
		{"truncated json payload", esEventFrame("x", `{"a":`), errEventStreamMalformed},
		{"exception without type", encodeESFrame(esStringHeader(":message-type", "exception"), []byte(`{}`)), errEventStreamMalformed},
		{"exception bad type", encodeESFrame(append(esStringHeader(":message-type", "exception"),
			esStringHeader(":exception-type", "bad type")...), []byte(`{}`)), errEventStreamMalformed},
		{"error without code", encodeESFrame(esStringHeader(":message-type", "error"), []byte(`{}`)), errEventStreamMalformed},
		{"event type header with wrong value type", encodeESFrame(append(esStringHeader(":message-type", "event"),
			esRawHeader(":event-type", 6, []byte{0, 1, 'x'})...), []byte(`{}`)), errEventStreamMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := esReadAll(tt.in)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if got != "" {
				t.Errorf("output %q must be empty on error", got)
			}
		})
	}
}

func TestEventStreamReader_TooLargeDoesNotReadPayload(t *testing.T) {
	t.Parallel()
	// The prelude announces 1 MiB + 1 but no body follows: reaching the body
	// read would produce a truncation error, not a size error.
	src := &esCountingReader{r: bytes.NewReader(esPrelude(maxEventStreamMessageSize+1, 0))}
	r := newEventStreamReader(io.NopCloser(src))
	_, err := io.ReadAll(r)
	if !errors.Is(err, errEventStreamTooLarge) {
		t.Fatalf("err = %v, want too large", err)
	}
	if src.n != eventStreamPreludeSize {
		t.Errorf("read %d bytes from source, want exactly the %d byte prelude", src.n, eventStreamPreludeSize)
	}
	if cap(r.frame) != 0 {
		t.Errorf("frame buffer cap = %d, want 0 (no allocation before limit check)", cap(r.frame))
	}
}

func TestEventStreamReader_BadPreludeCRCDoesNotAllocate(t *testing.T) {
	t.Parallel()
	p := esPrelude(maxEventStreamMessageSize, 0)
	p[8] ^= 0xff
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(p)))
	if _, err := io.ReadAll(r); !errors.Is(err, errEventStreamPreludeCRC) {
		t.Fatalf("err = %v, want prelude crc", err)
	}
	if cap(r.frame) != 0 {
		t.Errorf("frame buffer cap = %d, want 0", cap(r.frame))
	}
}

func TestEventStreamReader_MaxSizeFrameAccepted(t *testing.T) {
	t.Parallel()
	hdr := append(esStringHeader(":message-type", "event"), esStringHeader(":event-type", "big")...)
	// payload: {"a":"xxx..."} sized so the whole frame is exactly the maximum.
	overhead := eventStreamPreludeSize + len(hdr) + 4 + len(`{"a":""}`)
	payload := `{"a":"` + strings.Repeat("x", maxEventStreamMessageSize-overhead) + `"}`
	frame := encodeESFrame(hdr, []byte(payload))
	if len(frame) != maxEventStreamMessageSize {
		t.Fatalf("test frame size = %d, want %d", len(frame), maxEventStreamMessageSize)
	}
	got, err := esReadAll(frame)
	if err != nil {
		t.Fatalf("max size frame rejected: %v", err)
	}
	if !strings.HasPrefix(got, `data: {"big":{"a":"xxx`) || !strings.HasSuffix(got, "\"}}\n\n") {
		t.Errorf("unexpected output shape (len %d)", len(got))
	}
}

func TestEventStreamReader_CorruptionDetectedPerByte(t *testing.T) {
	t.Parallel()
	valid := esEventFrame("contentBlockDelta", `{"delta":{"text":"hello"}}`)
	for i := range valid {
		mut := bytes.Clone(valid)
		mut[i] ^= 0x01
		got, err := esReadAll(mut)
		if err == nil {
			t.Errorf("byte %d flipped: decoded without error (%q)", i, got)
		}
		if got != "" {
			t.Errorf("byte %d flipped: output %q must be empty", i, got)
		}
	}
}

func TestEventStreamReader_ErrorAfterGoodFramesDeliversGoodOutputFirst(t *testing.T) {
	t.Parallel()
	var in bytes.Buffer
	in.Write(esEventFrame("messageStart", `{}`))
	bad := esEventFrame("messageStop", `{}`)
	bad[len(bad)-1] ^= 0xff
	in.Write(bad)

	got, err := esReadAll(in.Bytes())
	if !errors.Is(err, errEventStreamMessageCRC) {
		t.Fatalf("err = %v, want message crc", err)
	}
	if got != "data: {\"messageStart\":{}}\n\n" {
		t.Errorf("good frame output lost: %q", got)
	}
}

func TestEventStreamReader_StickyError(t *testing.T) {
	t.Parallel()
	bad := esEventFrame("x", `{}`)
	bad[len(bad)-1] ^= 0xff
	// A valid frame follows the corrupt one and must never be decoded.
	stream := append(bytes.Clone(bad), esEventFrame("y", `{}`)...)
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(stream)))
	buf := make([]byte, 64)
	for i := 0; i < 3; i++ {
		n, err := r.Read(buf)
		if n != 0 || !errors.Is(err, errEventStreamMessageCRC) {
			t.Fatalf("read %d = %d, %v; want 0, message crc", i, n, err)
		}
	}
}

func TestEventStreamReader_ErrorTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		headers []byte
		want    string
	}{
		{"error code", append(esStringHeader(":message-type", "error"), esStringHeader(":error-code", "InternalFailure")...),
			"data: {\":exception\":\"InternalFailure\"}\n\n"},
		{"exception", append(esStringHeader(":message-type", "exception"), esStringHeader(":exception-type", "validationException")...),
			"data: {\":exception\":\"validationException\"}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := append(bytes.Clone(tt.headers), esStringHeader(":error-message", "prompt text secret-account-123")...)
			got, err := esReadAll(encodeESFrame(h, []byte(`{"message":"user said: my password is hunter2"}`)))
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			for _, leak := range []string{"hunter2", "secret-account", "prompt text", "password"} {
				if strings.Contains(got, leak) {
					t.Errorf("output leaks %q", leak)
				}
			}
		})
	}
}

func TestEventStreamReader_PayloadCompaction(t *testing.T) {
	t.Parallel()
	got, err := esReadAll(esEventFrame("contentBlockDelta", "{\r\n\t\"delta\" : { \"text\" : \"a\\nb\" } ,\n \"p\": [1, 2]\n}\n"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := "data: {\"contentBlockDelta\":{\"delta\":{\"text\":\"a\\nb\"},\"p\":[1,2]}}\n\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Count(got, "\n") != 2 {
		t.Errorf("output must contain exactly one SSE event (two newlines), got %q", got)
	}
}

func TestEventStreamReader_NonObjectPayloadStillOneEvent(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`"s"`, `1`, `null`, `[1]`, `true`} {
		got, err := esReadAll(esEventFrame("x", payload))
		if err != nil {
			t.Errorf("%s: %v", payload, err)
			continue
		}
		if got != "data: {\"x\":"+payload+"}\n\n" {
			t.Errorf("%s: got %q", payload, got)
		}
	}
}

func TestParseEventStreamHeaders_EveryTypeKeepsOnlyStringValues(t *testing.T) {
	t.Parallel()
	types := []struct {
		name string
		typ  byte
		val  []byte
	}{
		{"bool true", 0, nil},
		{"bool false", 1, nil},
		{"byte", 2, []byte{7}},
		{"int16", 3, []byte{0, 7}},
		{"int32", 4, []byte{0, 0, 0, 7}},
		{"int64", 5, make([]byte, 8)},
		{"bytes", 6, []byte{0, 3, 'a', 'b', 'c'}},
		{"timestamp", 8, make([]byte, 8)},
		{"uuid", 9, make([]byte, 16)},
		{"empty bytes", 6, []byte{0, 0}},
		{"empty string", 7, []byte{0, 0}},
	}
	for _, tt := range types {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := esRawHeader("x-first", tt.typ, tt.val)
			b = append(b, esStringHeader(":message-type", "event")...)
			b = append(b, esRawHeader("x-last", tt.typ, tt.val)...)
			b = append(b, esStringHeader(":event-type", "ev")...)
			h, err := parseEventStreamHeaders(b)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if h.messageType != "event" || h.eventType != "ev" {
				t.Errorf("headers = %+v", h)
			}
		})
	}

	t.Run("exception and error code retained", func(t *testing.T) {
		t.Parallel()
		b := append(esStringHeader(":exception-type", "E"), esStringHeader(":error-code", "C")...)
		b = append(b, esStringHeader(":error-message", "must not be kept")...)
		h, err := parseEventStreamHeaders(b)
		if err != nil {
			t.Fatal(err)
		}
		if h.exceptionType != "E" || h.errorCode != "C" {
			t.Errorf("headers = %+v", h)
		}
		if strings.Contains(h.messageType+h.eventType+h.exceptionType+h.errorCode, "kept") {
			t.Error(":error-message value retained")
		}
	})

	t.Run("non-string tracked header counts for duplicates", func(t *testing.T) {
		t.Parallel()
		b := esRawHeader(":event-type", 2, []byte{1})
		b = append(b, esStringHeader(":event-type", "x")...)
		if _, err := parseEventStreamHeaders(b); !errors.Is(err, errEventStreamMalformed) {
			t.Errorf("err = %v, want malformed", err)
		}
	})

	t.Run("empty headers", func(t *testing.T) {
		t.Parallel()
		h, err := parseEventStreamHeaders(nil)
		if err != nil || h != (eventStreamHeaders{}) {
			t.Errorf("got %+v, %v", h, err)
		}
	})
}

func TestEventStreamReader_ClosePropagatesError(t *testing.T) {
	t.Parallel()
	want := errors.New("close failed")
	r := newEventStreamReader(&esErrCloser{Reader: bytes.NewReader(nil), err: want})
	if err := r.Close(); !errors.Is(err, want) {
		t.Errorf("Close() = %v, want %v", err, want)
	}
}

func TestEventStreamReader_SourceReadError(t *testing.T) {
	t.Parallel()
	boom := errors.New("network reset")
	frame := esEventFrame("x", `{}`)
	r := newEventStreamReader(io.NopCloser(io.MultiReader(bytes.NewReader(frame[:5]), iotest.ErrReader(boom))))
	_, err := io.ReadAll(r)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want source error", err)
	}
}

type esCountingReader struct {
	r *bytes.Reader
	n int
}

func (c *esCountingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

type esErrCloser struct {
	io.Reader
	err error
}

func (c *esErrCloser) Close() error { return c.err }

// FuzzEventStreamReader asserts that arbitrary input never panics and that the
// output is always made of complete "data: " events separated by blank lines.
// Both a plain reader and a HalfReader are exercised and must agree.
func FuzzEventStreamReader(f *testing.F) {
	hdr := func(typ string) []byte {
		return append(esStringHeader(":message-type", "event"), esStringHeader(":event-type", typ)...)
	}
	exc := append(esStringHeader(":message-type", "exception"), esStringHeader(":exception-type", "throttlingException")...)
	errh := append(esStringHeader(":message-type", "error"), esStringHeader(":error-code", "Boom")...)

	seeds := [][]byte{
		nil,
		esEventFrame("messageStart", `{"role":"assistant"}`),
		esEventFrame("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hi"}}`),
		esEventFrame("metadata", `{"usage":{"inputTokens":1,"outputTokens":2}}`),
		encodeESFrame(exc, []byte(`{"message":"secret"}`)),
		encodeESFrame(errh, []byte(`{}`)),
		encodeESFrame(hdr("x"), []byte("{\n\"a\": 1\n}")),
		encodeESFrame(append(hdr("x"), esRawHeader("n", 9, make([]byte, 16))...), []byte(`{}`)),
		append(esEventFrame("messageStart", `{}`), esEventFrame("messageStop", `{"stopReason":"end_turn"}`)...),
		esEventFrame("messageStart", `{}`)[:20],
		esPrelude(16, 0),
		esPrelude(maxEventStreamMessageSize+1, 0),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	check := func(t *testing.T, out []byte) {
		t.Helper()
		if len(out) == 0 {
			return
		}
		if !bytes.HasSuffix(out, []byte("\n\n")) {
			t.Fatalf("output does not end with a blank line: %q", out)
		}
		for _, ev := range bytes.Split(bytes.TrimSuffix(out, []byte("\n\n")), []byte("\n\n")) {
			if !bytes.HasPrefix(ev, []byte("data: ")) {
				t.Fatalf("event without data prefix: %q", ev)
			}
			if bytes.Contains(ev, []byte("\n")) {
				t.Fatalf("event spans multiple lines: %q", ev)
			}
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		plain, plainErr := io.ReadAll(newEventStreamReader(io.NopCloser(bytes.NewReader(data))))
		check(t, plain)

		half, halfErr := io.ReadAll(newEventStreamReader(io.NopCloser(iotest.HalfReader(bytes.NewReader(data)))))
		check(t, half)

		if !bytes.Equal(plain, half) {
			t.Fatalf("HalfReader output differs:\nplain %q\nhalf  %q", plain, half)
		}
		if (plainErr == nil) != (halfErr == nil) {
			t.Fatalf("error mismatch: plain %v, half %v", plainErr, halfErr)
		}
	})
}

func TestParseEventStreamHeaders_DuplicateExceptionAndErrorCode(t *testing.T) {
	t.Parallel()

	for _, name := range []string{":exception-type", ":error-code"} {
		h := append(esStringHeader(name, "A"), esStringHeader(name, "B")...)
		if _, err := parseEventStreamHeaders(h); err == nil {
			t.Errorf("duplicate %s must be rejected", name)
		}
	}
}

func TestParseEventStreamHeaders_WrongTypedFirstThenStringDuplicate(t *testing.T) {
	t.Parallel()

	for _, name := range []string{":exception-type", ":error-code"} {
		h := append(esRawHeader(name, 4, []byte{0, 0, 0, 1}), esStringHeader(name, "B")...)
		if _, err := parseEventStreamHeaders(h); err == nil {
			t.Errorf("wrong-typed %s followed by a string duplicate must be rejected", name)
		}
	}
}
