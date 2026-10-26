package logbody

import "testing"

func TestLegacySnapshotsSplitIntoMetadataAndBody(t *testing.T) {
	for name, raw := range map[string]string{
		"text":      `{"body":{"byte_length":5,"text":"hello"},"headers":{"a":"b"},"method":"POST","url":"https://x"}`,
		"truncated": `{"body":{"byte_length":9,"text":"hel","truncated":true},"headers":{},"status":200,"status_code":200}`,
		"binary":    `{"body":{"base64":"/wE=","byte_length":4,"encoding":"base64","truncated":true},"headers":{}}`,
		"omitted":   `{"body":{"byte_length":4,"cleared":true},"headers":{},"method":"GET"}`,
		"no body":   `{"headers":{"x":"y"},"method":"GET","url":"https://x"}`,
	} {
		snapshot, verbatim := FromLegacy([]byte(raw), false)
		if verbatim {
			t.Errorf("%s: kept verbatim, want split", name)
			continue
		}
		if !sameJSON(snapshot.JSON(), []byte(raw)) {
			t.Errorf("%s: rendered %s, want %s", name, snapshot.JSON(), raw)
		}
		if snapshot.Body != nil && string(snapshot.Meta) == raw {
			t.Errorf("%s: the body was left inside the metadata", name)
		}
	}
}

// A shape this package does not write is kept whole, so it renders exactly as
// it did rather than as a guess.
func TestUnknownLegacyShapesAreKeptVerbatim(t *testing.T) {
	for name, raw := range map[string]string{
		"string body":   `{"body":"plain text","method":"GET"}`,
		"size field":    `{"body":{"base64_truncated":"AAAA","size":3},"method":"GET"}`,
		"unknown field": `{"body":{"byte_length":1,"text":"x","note":"?"},"method":"GET"}`,
		"false flag":    `{"body":{"byte_length":1,"text":"x","truncated":false},"method":"GET"}`,
		"not an object": `[1,2]`,
	} {
		snapshot, verbatim := FromLegacy([]byte(raw), false)
		if !verbatim || snapshot.Body != nil {
			t.Errorf("%s: split, want it kept verbatim", name)
			continue
		}
		if !sameJSON(snapshot.JSON(), []byte(raw)) {
			t.Errorf("%s: rendered %s, want %s", name, snapshot.JSON(), raw)
		}
	}
}

// A row whose bodies were already cleared keeps only its metadata, and reads
// back as it did.
func TestClearedLegacySnapshotsKeepOnlyMetadata(t *testing.T) {
	raw := `{"body":{"cleared":true},"headers":{"a":"b"},"status":200}`
	snapshot, verbatim := FromLegacy([]byte(raw), true)
	if verbatim || snapshot.Body != nil {
		t.Fatalf("verbatim=%v body=%v, want metadata only", verbatim, snapshot.Body)
	}
	if !sameJSON(RenderCleared(snapshot.Meta), []byte(raw)) {
		t.Errorf("rendered %s, want %s", RenderCleared(snapshot.Meta), raw)
	}
}
