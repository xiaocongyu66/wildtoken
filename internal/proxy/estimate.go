package proxy

import (
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"
)

// estimateTextTokens approximates a tokenizer: about four ASCII characters per
// token and one per other character. Close for English and CJK, high for
// accented Latin, which is the safer side for a quota.
func estimateTextTokens(text string) int64 {
	var ascii, other int64
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// estimateRequestTokens approximates a request's prompt from its text. Inline
// data — a data: URL, or a long run with no whitespace, as base64 is — is
// skipped: an image costs its provider's rate, not its encoding's length.
func estimateRequestTokens(body []byte) int64 {
	var request any
	if err := json.Unmarshal(body, &request); err != nil {
		return estimateTextTokens(string(body))
	}

	var tokens int64
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			if strings.HasPrefix(typed, "data:") ||
				(len(typed) > 1024 && !strings.ContainsAny(typed, " \n\t")) {
				return
			}
			tokens += estimateTextTokens(typed)
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(request)
	return tokens
}

// clampTokens narrows an estimate to the log's column type.
func clampTokens(tokens int64) int32 {
	return int32(min(max(tokens, 0), math.MaxInt32))
}
