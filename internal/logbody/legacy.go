package logbody

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
)

// FromLegacy splits a snapshot stored whole as JSON, as logs were before bodies
// were stored by chunk, into metadata and body.
//
// cleared says the keep-count policy had already dropped the row's bodies;
// such a snapshot keeps only its metadata.
//
// The split is checked by rendering it back: anything that does not come out
// equal to the stored JSON — a body shape from an older release, a field this
// package does not know — is kept verbatim as metadata, so it still renders
// exactly as it did. verbatim reports that.
func FromLegacy(raw []byte, cleared bool) (snapshot *Snapshot, verbatim bool) {
	keep := &Snapshot{Meta: compact(raw)}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return keep, true
	}

	var body *Body
	if encoded, ok := fields["body"]; ok {
		delete(fields, "body")
		if !cleared {
			if body = legacyBody(encoded); body == nil {
				return keep, true
			}
		}
	}
	// Maps of RawMessage always marshal.
	meta, _ := json.Marshal(fields)
	split := &Snapshot{Meta: meta, Body: body}

	rendered := split.JSON()
	if cleared {
		rendered = RenderCleared(meta)
	}
	if !sameJSON(rendered, raw) {
		return keep, true
	}
	return split, false
}

// legacyBody reads the body shapes this package writes, or nil for any other.
func legacyBody(encoded json.RawMessage) *Body {
	var fields struct {
		Text       *string `json:"text"`
		Base64     *string `json:"base64"`
		Encoding   string  `json:"encoding"`
		ByteLength *int64  `json:"byte_length"`
		Truncated  bool    `json:"truncated"`
		Cleared    bool    `json:"cleared"`
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil || fields.ByteLength == nil {
		return nil
	}

	body := &Body{Length: *fields.ByteLength, Truncated: fields.Truncated}
	switch {
	case fields.Cleared && fields.Text == nil && fields.Base64 == nil:
		body.Kind = KindOmitted
	case fields.Text != nil && fields.Base64 == nil && !fields.Cleared:
		body.Kind, body.Data = KindText, []byte(*fields.Text)
	case fields.Base64 != nil && fields.Text == nil && !fields.Cleared && fields.Encoding == "base64":
		data, err := base64.StdEncoding.DecodeString(*fields.Base64)
		if err != nil {
			return nil
		}
		body.Kind, body.Data = KindBinary, data
	default:
		return nil
	}
	return body
}

// sameJSON compares two JSON documents by value.
func sameJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// compact strips insignificant whitespace, keeping a verbatim snapshot as
// small as it can be stored.
func compact(raw []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return raw
	}
	return out.Bytes()
}
