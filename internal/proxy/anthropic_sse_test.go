package proxy

import "testing"

// The Messages stream is split: message_start carries the input counts and
// message_delta the output. Reading them through the official SDK must reach
// the same figures the hand-written walker did.
func TestSDKReadsAnthropicSplitUsage(t *testing.T) {
	body := []byte(`data: {"type":"message_start","message":{"usage":` +
		`{"input_tokens":40,"cache_read_input_tokens":60,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n")

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(100), "completion": int32(9), "total": int32(109),
		"prompt_cached": int32(60),
	})
}

// A later event repeats the cumulative figures; its value replaces the earlier.
func TestSDKPrefersTheLatestCumulativeUsage(t *testing.T) {
	body := []byte(`data: {"type":"message_start","message":{"usage":` +
		`{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":25,"input_tokens":12}}` + "\n\n")

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(12), "completion": int32(25), "total": int32(37),
	})
}

// Thinking tokens are a breakdown of output_tokens, never added to it.
func TestSDKKeepsThinkingAsBreakdown(t *testing.T) {
	body := []byte(`data: {"type":"message_start","message":{"usage":` +
		`{"input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":503,` +
		`"output_tokens_details":{"thinking_tokens":312}}}` + "\n\n")

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(100), "completion": int32(503), "total": int32(603),
		"completion_reason": int32(312),
	})
}

func TestSDKSeesTextThinkingAndToolDeltasAsTheFirstToken(t *testing.T) {
	cases := map[string]string{
		"text": `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"text_delta","text":"hi"}}`,
		"thinking": `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"thinking_delta","thinking":"hm"}}`,
		"tool input": `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"input_json_delta","partial_json":"{"}}`,
	}
	for name, line := range cases {
		if !HasVisibleToken([]byte(line)) {
			t.Errorf("%s delta was not counted as the first token", name)
		}
	}

	for name, line := range map[string]string{
		"signature": `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"signature_delta","signature":"abc"}}`,
		"stop": `data: {"type":"content_block_stop","index":0}`,
		"empty text": `data: {"type":"content_block_delta","index":0,` +
			`"delta":{"type":"text_delta","text":""}}`,
	} {
		if HasVisibleToken([]byte(line)) {
			t.Errorf("%s was counted as the first token", name)
		}
	}
}

// A LiteLLM-style payload keeps OpenAI's inclusive prompt_tokens beside
// Anthropic's cache fields. It is not a Messages event, so the generic walker
// must keep handling it and must not add the cache a second time.
func TestSDKLeavesHybridPayloadsToTheGenericWalker(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":12012,"completion_tokens":5,"total_tokens":12017,
        "prompt_tokens_details":{"cached_tokens":12000},
        "cache_creation_input_tokens":0,"cache_read_input_tokens":12000}}`)

	assertUsage(t, ExtractUsage(body, "application/json"), map[string]any{
		"prompt": int32(12012), "completion": int32(5), "total": int32(12017),
		"prompt_cached": int32(12000), "cache_creation": int32(0),
	})
}

// The observation path must agree with the buffered one, since it is what
// bills a live request.
func TestSDKObservationMatchesBufferedExtraction(t *testing.T) {
	body := `data: {"type":"message_start","message":{"usage":` +
		`{"input_tokens":40,"cache_read_input_tokens":60,"output_tokens":1}}}` + "\n\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n"

	observation := &sseObservation{}
	observation.observeChunk([]byte(body), func() int32 { return 12 })
	observation.finish(func() int32 { return 12 })

	usage := observation.tokenUsage()
	if usage.PromptTokens == nil || *usage.PromptTokens != 100 {
		t.Fatalf("prompt = %v, want 100", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 9 {
		t.Fatalf("completion = %v, want 9", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 109 {
		t.Fatalf("total = %v, want 109", usage.TotalTokens)
	}
	if usage.PromptCachedTokens == nil || *usage.PromptCachedTokens != 60 {
		t.Fatalf("prompt cached = %v, want 60", usage.PromptCachedTokens)
	}
}

// A malformed event of a known type is still an Anthropic event: it must not
// fall through and be read as an OpenAI payload.
func TestSDKIgnoresMalformedAnthropicEvents(t *testing.T) {
	if usage := ExtractUsage([]byte("data: {\"type\":\"message_delta\"\n\n"),
		"text/event-stream"); usage.TotalTokens != nil {
		t.Fatalf("malformed event produced usage %v", usage)
	}
}
