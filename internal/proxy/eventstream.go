package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"regexp"
)

const (
	// maxEventStreamMessageSize is the largest accepted total frame size.
	maxEventStreamMessageSize = 1 << 20
	// maxEventStreamHeadersSize is the largest accepted headers section.
	maxEventStreamHeadersSize = 128 << 10
	// eventStreamPreludeSize is the size of total length, headers length and prelude CRC.
	eventStreamPreludeSize = 12
	// eventStreamMinMessageSize is the smallest valid frame: prelude plus message CRC.
	eventStreamMinMessageSize = 16
)

// Event-stream decoding errors. They are static strings and never carry
// payload bytes, so they are safe to log.
var (
	errEventStreamPreludeCRC = errors.New("eventstream: prelude checksum mismatch")
	errEventStreamMessageCRC = errors.New("eventstream: message checksum mismatch")
	errEventStreamTooLarge   = errors.New("eventstream: message exceeds size limit")
	errEventStreamTruncated  = errors.New("eventstream: truncated message")
	errEventStreamMalformed  = errors.New("eventstream: malformed message")
)

// eventStreamNameRe restricts event, exception and error-code names so they
// can be embedded into JSON without escaping.
var eventStreamNameRe = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

// eventStreamReader decodes the AWS binary event-stream wire format and
// re-emits every frame as one SSE event of the form "data: <json>\n\n".
// It is not safe for concurrent use.
type eventStreamReader struct {
	src   io.ReadCloser
	frame []byte       // reused, grows to at most maxEventStreamMessageSize
	out   bytes.Buffer // translated, not yet returned bytes
	err   error        // sticky
}

// eventStreamHeaders holds the only header values the decoder retains.
// The :error-message header is deliberately never kept.
type eventStreamHeaders struct {
	messageType   string
	eventType     string
	exceptionType string
	errorCode     string
}

// newEventStreamReader wraps src, which must yield AWS event-stream frames.
func newEventStreamReader(src io.ReadCloser) *eventStreamReader {
	return &eventStreamReader{src: src}
}

// Read implements io.Reader. It returns translated SSE bytes. Buffered output
// is always delivered before a sticky error is reported.
func (r *eventStreamReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.out.Len() == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if err := r.readMessage(); err != nil {
			r.err = err
		}
	}
	return r.out.Read(p)
}

// Close closes the underlying source.
func (r *eventStreamReader) Close() error {
	return r.src.Close()
}

// readMessage reads exactly one frame from the source and appends its SSE
// translation to the output buffer. It returns io.EOF on a clean end of stream.
func (r *eventStreamReader) readMessage() error {
	var prelude [eventStreamPreludeSize]byte
	n, err := io.ReadFull(r.src, prelude[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return io.EOF
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errEventStreamTruncated
		}
		return err
	}

	if crc32.ChecksumIEEE(prelude[0:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return errEventStreamPreludeCRC
	}

	total := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if total < eventStreamMinMessageSize || total > maxEventStreamMessageSize {
		if total > maxEventStreamMessageSize {
			return errEventStreamTooLarge
		}
		return errEventStreamMalformed
	}
	if headersLen > maxEventStreamHeadersSize {
		return errEventStreamTooLarge
	}
	if headersLen > total-eventStreamMinMessageSize {
		return errEventStreamMalformed
	}

	size := int(total)
	if cap(r.frame) < size {
		r.frame = make([]byte, size)
	}
	frame := r.frame[:size]
	copy(frame, prelude[:])
	if _, err := io.ReadFull(r.src, frame[eventStreamPreludeSize:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errEventStreamTruncated
		}
		return err
	}

	if crc32.ChecksumIEEE(frame[:size-4]) != binary.BigEndian.Uint32(frame[size-4:]) {
		return errEventStreamMessageCRC
	}

	headersEnd := eventStreamPreludeSize + int(headersLen)
	hdrs, err := parseEventStreamHeaders(frame[eventStreamPreludeSize:headersEnd])
	if err != nil {
		return err
	}
	payload := frame[headersEnd : size-4]

	switch hdrs.messageType {
	case "event":
		if !eventStreamNameRe.MatchString(hdrs.eventType) {
			return errEventStreamMalformed
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, payload); err != nil {
			return errEventStreamMalformed
		}
		r.out.WriteString(`data: {"`)
		r.out.WriteString(hdrs.eventType)
		r.out.WriteString(`":`)
		r.out.Write(compact.Bytes())
		r.out.WriteString("}\n\n")
	case "exception":
		if !eventStreamNameRe.MatchString(hdrs.exceptionType) {
			return errEventStreamMalformed
		}
		writeEventStreamException(&r.out, hdrs.exceptionType)
	case "error":
		if !eventStreamNameRe.MatchString(hdrs.errorCode) {
			return errEventStreamMalformed
		}
		writeEventStreamException(&r.out, hdrs.errorCode)
	default:
		return errEventStreamMalformed
	}
	return nil
}

// writeEventStreamException emits the content-free exception marker event.
// The name has already been validated against eventStreamNameRe.
func writeEventStreamException(w *bytes.Buffer, name string) {
	w.WriteString(`data: {":exception":"`)
	w.WriteString(name)
	w.WriteString("\"}\n\n")
}

// parseEventStreamHeaders parses a headers section. Every value type 0-9 is
// parsed or skipped; only the string values of :message-type, :event-type,
// :exception-type and :error-code are retained.
func parseEventStreamHeaders(b []byte) (eventStreamHeaders, error) {
	var h eventStreamHeaders
	var seenMessageType, seenEventType, seenExceptionType, seenErrorCode bool

	for len(b) > 0 {
		nameLen := int(b[0])
		b = b[1:]
		if nameLen < 1 || nameLen > len(b) {
			return eventStreamHeaders{}, errEventStreamMalformed
		}
		name := string(b[:nameLen])
		b = b[nameLen:]
		if len(b) < 1 {
			return eventStreamHeaders{}, errEventStreamMalformed
		}
		valueType := b[0]
		b = b[1:]

		var fixed int
		var variable bool
		switch valueType {
		case 0, 1:
			fixed = 0
		case 2:
			fixed = 1
		case 3:
			fixed = 2
		case 4:
			fixed = 4
		case 5, 8:
			fixed = 8
		case 9:
			fixed = 16
		case 6, 7:
			variable = true
		default:
			return eventStreamHeaders{}, errEventStreamMalformed
		}

		var value []byte
		if variable {
			if len(b) < 2 {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			l := int(binary.BigEndian.Uint16(b[:2]))
			b = b[2:]
			if l > len(b) {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			value = b[:l]
			b = b[l:]
		} else {
			if fixed > len(b) {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			b = b[fixed:]
		}

		if valueType != 7 {
			// Duplicate detection still applies to the tracked names
			// regardless of the (wrong) value type.
			switch name {
			case ":message-type":
				if seenMessageType {
					return eventStreamHeaders{}, errEventStreamMalformed
				}
				seenMessageType = true
			case ":event-type":
				if seenEventType {
					return eventStreamHeaders{}, errEventStreamMalformed
				}
				seenEventType = true
			case ":exception-type":
				if seenExceptionType {
					return eventStreamHeaders{}, errEventStreamMalformed
				}
				seenExceptionType = true
			case ":error-code":
				if seenErrorCode {
					return eventStreamHeaders{}, errEventStreamMalformed
				}
				seenErrorCode = true
			}
			continue
		}

		switch name {
		case ":message-type":
			if seenMessageType {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			seenMessageType = true
			h.messageType = string(value)
		case ":event-type":
			if seenEventType {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			seenEventType = true
			h.eventType = string(value)
		case ":exception-type":
			if seenExceptionType {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			seenExceptionType = true
			h.exceptionType = string(value)
		case ":error-code":
			if seenErrorCode {
				return eventStreamHeaders{}, errEventStreamMalformed
			}
			seenErrorCode = true
			h.errorCode = string(value)
		}
	}
	return h, nil
}
