package logbody

import (
	"bytes"
	"unicode/utf8"
)

// BodyKind says how a captured body renders. The values are stored in body
// records, so they never change meaning.
type BodyKind uint8

const (
	// KindNone is a snapshot without a body key.
	KindNone BodyKind = 0
	// KindText is valid UTF-8, rendered as {"text": ...}.
	KindText BodyKind = 1
	// KindBinary is anything else, rendered as base64.
	KindBinary BodyKind = 2
	// KindOmitted is a body the capture budget of 0 left out; only its length
	// is known.
	KindOmitted BodyKind = 3
)

// retainedSlack is how much of a caller's buffer a capture may keep alive
// beyond the bytes it holds. A capture that keeps the whole body shares the
// buffer; one that kept 1 MiB of a 50 MiB body copies, so the queued log does
// not hold the rest of the request in memory.
const retainedSlack = 64 << 10

// Body is a captured request or response body.
type Body struct {
	Kind BodyKind
	// Data is the captured prefix. It may share the caller's buffer and is
	// never modified.
	Data []byte
	// Length is the length of the whole body, captured or not.
	Length    int64
	Truncated bool
}

// CaptureBody keeps up to budget bytes of a body whose full length is
// originalLength; body may already be only a prefix of it.
//
// Text is cut on a UTF-8 boundary and kept as text; anything that is not UTF-8
// is kept as binary. A budget of 0 keeps nothing but the length.
func CaptureBody(body []byte, budget, originalLength int) *Body {
	if budget == 0 {
		return &Body{Kind: KindOmitted, Length: int64(originalLength)}
	}
	if len(body) == 0 {
		return &Body{Kind: KindText, Data: []byte{}, Length: int64(originalLength)}
	}

	slice := body[:min(len(body), budget)]
	if text, ok := validTextPrefix(body, slice, originalLength); ok {
		return &Body{
			Kind:      KindText,
			Data:      shed(text),
			Length:    int64(originalLength),
			Truncated: len(text) < originalLength,
		}
	}
	return &Body{
		Kind:      KindBinary,
		Data:      shed(slice),
		Length:    int64(originalLength),
		Truncated: len(slice) < originalLength,
	}
}

// validTextPrefix returns the longest valid UTF-8 prefix of slice, or ok=false
// when the body is not text at all.
func validTextPrefix(body, slice []byte, originalLength int) ([]byte, bool) {
	if len(body) == originalLength {
		// The whole body is present, so it can be validated as one string before
		// the prefix is cut back to a rune boundary. An index equal to the length
		// is already a boundary, which is why the guard stops there.
		if !utf8.Valid(body) {
			return nil, false
		}
		cutoff := len(slice)
		for cutoff > 0 && cutoff < len(body) && !utf8.RuneStart(body[cutoff]) {
			cutoff--
		}
		return body[:cutoff], true
	}

	// Only a prefix of a longer body is available. A trailing partial rune is
	// expected here, so up to UTFMax-1 bytes of it are dropped; anything invalid
	// before that is binary, as it is for a whole body. Backing off one byte at
	// a time instead revalidated the prefix at every step: a stray byte midway
	// through a 1 MiB capture cost 13 seconds.
	for back := 0; back < utf8.UTFMax && back <= len(slice); back++ {
		if prefix := slice[:len(slice)-back]; utf8.Valid(prefix) {
			if len(prefix) == 0 && len(slice) > 0 {
				return nil, false
			}
			return prefix, true
		}
	}
	return nil, false
}

// shed copies a capture that would otherwise keep a much larger buffer alive.
func shed(data []byte) []byte {
	if cap(data) > 2*len(data)+retainedSlack {
		return bytes.Clone(data)
	}
	return data
}
