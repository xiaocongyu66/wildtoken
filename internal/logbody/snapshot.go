package logbody

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

// Snapshot is one captured request or response.
//
// Meta is the snapshot's JSON object without its body — method, url and
// headers, or status and headers — and Body the body as bytes, so storage can
// cut it into chunks instead of escaping it into JSON. A nil Body means the
// snapshot has no body key at all.
//
// A snapshot is never modified once built: a request retried on several
// channels logs the same snapshot on every row.
type Snapshot struct {
	Meta []byte
	Body *Body
}

// BodyBytes is how many captured body bytes the snapshot keeps in memory.
func (s *Snapshot) BodyBytes() int {
	if s == nil || s.Body == nil {
		return 0
	}
	return len(s.Body.Data)
}

// WithoutBody returns a copy whose body keeps only its length, as a capture
// budget of 0 would have. The original is left alone, since other rows may
// share it.
func (s *Snapshot) WithoutBody() *Snapshot {
	if s == nil || s.Body == nil || s.Body.Kind == KindOmitted {
		return s
	}
	return &Snapshot{Meta: s.Meta, Body: &Body{Kind: KindOmitted, Length: s.Body.Length}}
}

// JSON renders the snapshot as the console reads it.
func (s *Snapshot) JSON() json.RawMessage {
	if s == nil {
		return nil
	}
	return Render(s.Meta, s.Body)
}

// Render joins metadata and a body into one snapshot object:
//
//	{"body":{"byte_length":5,"text":"hello"},"headers":{...},"method":"POST",...}
//
// A nil body renders the metadata as stored. Every metadata key sorts after
// "body", so the body goes first, as encoding/json would have put it.
func Render(meta []byte, body *Body) json.RawMessage {
	if body == nil {
		return meta
	}
	encoded := renderBody(body)

	var out bytes.Buffer
	out.Grow(len(encoded) + len(meta) + 10)
	out.WriteString(`{"body":`)
	out.Write(encoded)
	if rest := bytes.TrimSpace(meta); len(rest) > 2 {
		out.WriteByte(',')
		out.Write(rest[1:])
	} else {
		out.WriteByte('}')
	}
	return out.Bytes()
}

// renderBody encodes a body in the shape the console reads:
//
//	{"byte_length":N,"text":"..."}
//	{"base64":"...","byte_length":N,"encoding":"base64"}
//	{"byte_length":N,"cleared":true}
//
// with "truncated":true added when only a prefix was kept.
func renderBody(body *Body) []byte {
	fields := map[string]any{"byte_length": body.Length}
	switch body.Kind {
	case KindText:
		fields["text"] = string(body.Data)
	case KindBinary:
		fields["base64"] = base64.StdEncoding.EncodeToString(body.Data)
		fields["encoding"] = "base64"
	case KindOmitted:
		fields["cleared"] = true
	}
	if body.Truncated && body.Kind != KindOmitted {
		fields["truncated"] = true
	}
	// Strings, numbers and booleans always marshal.
	encoded, _ := json.Marshal(fields)
	return encoded
}

// clearedSnapshot stands in for metadata that no longer parses.
const clearedSnapshot = `{"body":{"cleared":true}}`

// RenderCleared renders a snapshot whose body the keep-count policy dropped:
// the metadata stays and the body reads {"cleared":true}, whatever it was.
func RenderCleared(meta []byte) json.RawMessage {
	// A JSON null decodes without error into a nil map, and assigning to that
	// would panic.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(meta, &decoded); err != nil || decoded == nil {
		return json.RawMessage(clearedSnapshot)
	}
	decoded["body"] = json.RawMessage(`{"cleared":true}`)
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return json.RawMessage(clearedSnapshot)
	}
	return encoded
}
