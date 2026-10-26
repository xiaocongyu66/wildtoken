package logbody

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// bodyJSON renders a captured body the way a snapshot carries it.
func bodyJSON(t *testing.T, body *Body) map[string]any {
	t.Helper()
	var snapshot struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(Render([]byte(`{}`), body), &snapshot); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return snapshot.Body
}

func TestTextIsTruncatedAtUTF8Boundary(t *testing.T) {
	body := bodyJSON(t, CaptureBody([]byte("aéz"), 2, 4))
	if body["text"] != "a" {
		t.Errorf("text = %v, want the prefix cut back to a rune boundary", body["text"])
	}
	if body["byte_length"] != float64(4) {
		t.Errorf("byte_length = %v, want 4", body["byte_length"])
	}
	if body["truncated"] != true {
		t.Error("truncated was not reported")
	}
}

// A captured prefix may end mid-rune; that tail is dropped. An invalid byte
// before it makes the capture binary, found without revalidating the prefix at
// every byte: that took 13 seconds for a 1 MiB capture.
func TestAPrefixCaptureIsCheckedInLinearTime(t *testing.T) {
	if partial := CaptureBody([]byte("a\xc3"), 2, 10); string(partial.Data) != "a" {
		t.Errorf("text = %q, want the partial rune dropped", partial.Data)
	}

	capture := bytes.Repeat([]byte("x"), 1<<20)
	capture[len(capture)/2] = 0xff
	started := time.Now()
	body := CaptureBody(capture, len(capture), len(capture)+10)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if body.Kind != KindBinary {
		t.Errorf("a capture with an invalid byte midway was kept as text")
	}
}

func TestBinaryIsSlicedBeforeBase64Encoding(t *testing.T) {
	body := bodyJSON(t, CaptureBody([]byte{0xff, 1, 2, 3}, 2, 4))
	if body["base64"] != "/wE=" || body["encoding"] != "base64" {
		t.Errorf("body = %v, want base64 /wE=", body)
	}
	if body["byte_length"] != float64(4) || body["truncated"] != true {
		t.Errorf("body = %v, want 4 bytes reported truncated", body)
	}

	// The whole body is present and is not valid text, so it is binary even
	// though the budget cuts before the invalid byte.
	invalidAfterBudget := bodyJSON(t, CaptureBody([]byte{'a', 0xff}, 1, 2))
	if invalidAfterBudget["base64"] != "YQ==" || invalidAfterBudget["byte_length"] != float64(2) {
		t.Errorf("body = %v, want base64 YQ== of 2 bytes", invalidAfterBudget)
	}
}

func TestZeroBudgetKeepsOnlyTheLength(t *testing.T) {
	body := bodyJSON(t, CaptureBody([]byte("body"), 0, 4))
	if body["cleared"] != true || body["byte_length"] != float64(4) {
		t.Errorf("body = %v, want it cleared with the original length", body)
	}
	if _, hasText := body["text"]; hasText {
		t.Error("a cleared body still carried text")
	}
}

// A capture that keeps the whole body shares the caller's buffer; one that kept
// a small prefix of a large buffer copies it, so a queued log does not hold the
// rest of a 50 MiB request in memory.
func TestASmallCaptureLetsTheLargeBufferGo(t *testing.T) {
	large := bytes.Repeat([]byte("x"), 1<<20)
	if whole := CaptureBody(large, len(large), len(large)); &whole.Data[0] != &large[0] {
		t.Error("a whole-body capture copied the buffer")
	}
	if prefix := CaptureBody(large, 1024, len(large)); &prefix.Data[0] == &large[0] || cap(prefix.Data) > 2048 {
		t.Error("a small capture kept the large buffer alive")
	}
}

// oldRenderedBody is how snapshots rendered a body before bodies were stored by
// chunk. Every captured body must render to the same JSON value it did then.
func oldRenderedBody(body []byte, budget, originalByteLength int) map[string]any {
	if budget == 0 {
		return map[string]any{"cleared": true, "byte_length": originalByteLength}
	}
	if len(body) == 0 {
		return map[string]any{"text": "", "byte_length": originalByteLength}
	}
	slice := body[:min(len(body), budget)]
	if text, ok := validTextPrefix(body, slice, originalByteLength); ok {
		snapshot := map[string]any{"text": string(text), "byte_length": originalByteLength}
		if len(text) < originalByteLength {
			snapshot["truncated"] = true
		}
		return snapshot
	}
	snapshot := map[string]any{
		"base64":      base64.StdEncoding.EncodeToString(slice),
		"encoding":    "base64",
		"byte_length": originalByteLength,
	}
	if len(slice) < originalByteLength {
		snapshot["truncated"] = true
	}
	return snapshot
}

func TestCapturedBodiesRenderAsTheyAlwaysHave(t *testing.T) {
	// A length of -1 is the whole body; anything else says body is a prefix.
	cases := []struct {
		name           string
		body           []byte
		budget, length int
	}{
		{"empty", []byte{}, 10, -1},
		{"empty prefix of a longer body", []byte{}, 10, 7},
		{"plain", []byte("hello"), 10, -1},
		{"cut", []byte("hello"), 3, -1},
		{"html", []byte(`<a href="x">&amp;</a>`), 100, -1},
		{"line separators", []byte("a b c"), 100, -1},
		{"controls", []byte("\b\f\n\r\t\x00\x1f"), 100, -1},
		{"multibyte cut", []byte("中文字符"), 7, -1},
		{"prefix ending mid-rune", []byte("ab\xe4\xb8"), 4, 20},
		{"invalid", []byte("a\xffb"), 100, -1},
		{"invalid past budget", []byte("ab\xff"), 2, -1},
		{"zero budget", []byte("secret"), 0, -1},
		{"json", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), 1 << 20, -1},
	}
	for _, c := range cases {
		if c.length < 0 {
			c.length = len(c.body)
		}
		got := Render([]byte(`{"method":"POST"}`), CaptureBody(c.body, c.budget, c.length))
		wantBody, _ := json.Marshal(oldRenderedBody(c.body, c.budget, c.length))
		want := `{"body":` + string(wantBody) + `,"method":"POST"}`
		if !sameJSON(got, []byte(want)) {
			t.Errorf("%s: rendered %s, want %s", c.name, got, want)
		}
		if !utf8.Valid(got) || !json.Valid(got) {
			t.Errorf("%s: rendered invalid JSON %q", c.name, got)
		}
	}
}

func TestRenderPutsTheBodyFirst(t *testing.T) {
	body := &Body{Kind: KindText, Data: []byte("x"), Length: 1}
	if got := string(Render([]byte(`{"headers":{},"method":"GET"}`), body)); !strings.HasPrefix(got, `{"body":{`) ||
		!strings.HasSuffix(got, `,"headers":{},"method":"GET"}`) {
		t.Errorf("rendered %s", got)
	}
	if got := string(Render([]byte(`{}`), body)); got != `{"body":{"byte_length":1,"text":"x"}}` {
		t.Errorf("rendered %s with empty metadata", got)
	}
	if got := string(Render([]byte(`{"status":200}`), nil)); got != `{"status":200}` {
		t.Errorf("a snapshot without a body rendered as %s", got)
	}
}

func TestRenderClearedKeepsMetadataAndSurvivesCorruption(t *testing.T) {
	cleared := RenderCleared([]byte(`{"headers":{"a":"b"},"status":200}`))
	if !sameJSON(cleared, []byte(`{"body":{"cleared":true},"headers":{"a":"b"},"status":200}`)) {
		t.Errorf("cleared = %s", cleared)
	}
	for _, broken := range []string{`not json`, `null`, ``} {
		if got := string(RenderCleared([]byte(broken))); got != clearedSnapshot {
			t.Errorf("%q rendered as %s, want %s", broken, got, clearedSnapshot)
		}
	}
}
