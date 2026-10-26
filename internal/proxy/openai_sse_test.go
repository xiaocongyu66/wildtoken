package proxy

import (
	"slices"
	"strings"
	"testing"
)

// A Chat Completions chunk is read with the official SDK: content and tool-call
// deltas count toward the first visible token, and the final chunk's usage is
// cumulative.
func TestSDKReadsChatChunkUsage(t *testing.T) {
	body := []byte(
		`data: {"id":"x","object":"chat.completion.chunk","type":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
			`data: {"id":"x","type":"chat.completion.chunk","choices":[],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,` +
			`"prompt_tokens_details":{"cached_tokens":4},` +
			`"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\n",
	)

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(10), "completion": int32(5), "total": int32(15),
		"prompt_cached": int32(4), "completion_reason": int32(2),
	})

	if !HasVisibleToken([]byte(`data: {"type":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}` + "\n")) {
		t.Error("a content delta was not counted as the first token")
	}
	if HasVisibleToken([]byte(`data: {"type":"chat.completion.chunk","choices":[{"delta":{}}]}` + "\n")) {
		t.Error("an empty delta was counted as the first token")
	}
}

// An Anthropic-style chunk is not an OpenAI event and must not be read by the
// OpenAI SDK path, even though both carry a `type` field.
func TestChatChunkIgnoresAnthropicStreams(t *testing.T) {
	body := []byte(`data: {"type":"content_block_delta","index":0,` +
		`"delta":{"type":"text_delta","text":"x"}}` + "\n\n")

	// content_block_delta is not a chat.completion event, so it stays on the
	// Anthropic SDK path.
	if !HasVisibleToken(body) {
		t.Error("an Anthropic content delta should count as a visible token")
	}
}

// The observation path for a live Chat stream agrees with the buffered one.
func TestSDKChatObservationMatchesBufferedExtraction(t *testing.T) {
	body := `data: {"type":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"type":"chat.completion.chunk","choices":[],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n"

	observation := &sseObservation{}
	observation.observeChunk([]byte(body), func() int32 { return 7 })
	observation.finish(func() int32 { return 7 })

	usage := observation.tokenUsage()
	if usage.PromptTokens == nil || *usage.PromptTokens != 3 {
		t.Fatalf("prompt = %v, want 3", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 2 {
		t.Fatalf("completion = %v, want 2", usage.CompletionTokens)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 5 {
		t.Fatalf("total = %v, want 5", usage.TotalTokens)
	}
	if observation.firstTokenMs == nil {
		t.Fatal("no first-token time was recorded")
	}
	if !observation.finalUsageSeen {
		t.Fatal("the final chunk with usage should mark final usage")
	}
}

// A failed response is a failure even when it carries no error object: the
// stream still began as a 200 and must not be logged as a success.
func TestAFailedResponseWithoutAnErrorObjectIsStillAFailure(t *testing.T) {
	observation := &sseObservation{}
	observation.observeChunk([]byte(`data: {"type":"response.failed"}`+"\n\n"),
		func() int32 { return 1 })
	observation.finish(func() int32 { return 1 })

	if observation.streamError == nil {
		t.Fatal("a failed response without an error object was not recorded as a failure")
	}
	if !observation.terminalEventSeen {
		t.Fatal("the failed event was not seen as terminal")
	}
}

// Some compatible upstreams put usage at the top level of a Responses event
// rather than under response.
func TestSDKReadsTopLevelResponsesUsage(t *testing.T) {
	body := []byte(`data: {"type":"response.completed","usage":` +
		`{"input_tokens":3,"output_tokens":2,"total_tokens":5}}` + "\n\n")

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(3), "completion": int32(2), "total": int32(5),
	})

	observation := &sseObservation{}
	observation.observeChunk(body, func() int32 { return 1 })
	observation.finish(func() int32 { return 1 })
	if !observation.finalUsageSeen {
		t.Fatal("a terminal top-level usage was not treated as final")
	}
}

// A Responses failed event whose error names only a type still blames the
// channel.
func TestAFailedResponseWithOnlyAnErrorTypeIsCharged(t *testing.T) {
	observation := &sseObservation{}
	observation.observeChunk([]byte(`data: {"type":"response.failed","response":`+
		`{"error":{"type":"server_error","message":"x"}}}`+"\n\n"),
		func() int32 { return 1 })
	observation.finish(func() int32 { return 1 })

	if observation.streamError == nil {
		t.Fatal("the failed response was not recorded")
	}
	if !observation.streamErrorFault {
		t.Fatal("a server_error type was not charged to the channel")
	}
}

// input_tokens_details is a Chat usage spelling the SDK does not model.
func TestChatInputTokensDetailsAliasesAreRead(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,` +
		`"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}}`)

	assertUsage(t, ExtractUsage(body, "application/json"), map[string]any{
		"prompt": int32(100), "completion": int32(50), "total": int32(150),
		"prompt_cached": int32(10), "cache_creation": int32(20),
	})
}

// A bare delta.text is still a visible token.
func TestSDKSeesDeltaTextAsTheFirstToken(t *testing.T) {
	line := []byte(`data: {"object":"chat.completion.chunk","choices":[{"delta":{"text":"hi"}}]}` + "\n")
	if !HasVisibleToken(line) {
		t.Fatal("delta.text was not counted as the first token")
	}
}

// A Responses body wrapped under response is still read.
func TestWrappedResponsesBodyIsRead(t *testing.T) {
	body := []byte(`{"response":{"reasoning":{"effort":"high"},` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)

	assertUsage(t, ExtractUsage(body, "application/json"), map[string]any{
		"prompt": int32(3), "completion": int32(2), "total": int32(5),
	})
	if effort := ExtractResponseReasoningEffort(body, "application/json"); effort == nil || *effort != "high" {
		t.Fatalf("effort = %v, want high", effort)
	}
}

// A Responses usage has input_tokens and a total but no prompt_tokens. The
// oversized-line recovery must read it as Responses, not as a Chat usage that
// only reports a total.
func TestOversizedResponsesUsageKeepsPromptAndCompletion(t *testing.T) {
	image := strings.Repeat("A", maxSSEEventBytes+1024)
	stream := []byte(`data: {"type":"response.image_generation_call.completed","b64_json":"` + image +
		`","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}` + "\n\n")

	var observation sseObservation
	for chunk := range slices.Chunk(stream, 32<<10) {
		observation.observeChunk(chunk, func() int32 { return 0 })
	}
	observation.finish(func() int32 { return 0 })

	assertUsage(t, observation.tokenUsage(), map[string]any{
		"prompt": int32(10), "completion": int32(20), "total": int32(30),
	})
}

// Responses puts the cache-write counter in input_tokens_details, not at the
// usage level. It must still be read.
func TestResponsesNestedCacheWriteIsRead(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,` +
		`"input_tokens_details":{"cached_tokens":10,"cache_write_tokens":20}}}`)

	assertUsage(t, ExtractUsage(body, "application/json"), map[string]any{
		"prompt": int32(100), "completion": int32(50), "total": int32(150),
		"prompt_cached": int32(10), "cache_creation": int32(20),
	})
}

// A failed response may still report what it spent before failing.
func TestAFailedResponseKeepsItsUsage(t *testing.T) {
	observation := &sseObservation{}
	observation.observeChunk([]byte(`data: {"type":"response.failed","response":`+
		`{"error":{"code":"server_error","message":"x"},`+
		`"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`+"\n\n"),
		func() int32 { return 1 })
	observation.finish(func() int32 { return 1 })

	if observation.streamError == nil {
		t.Fatal("the failed response was not recorded as a failure")
	}
	assertUsage(t, observation.tokenUsage(), map[string]any{
		"prompt": int32(4), "completion": int32(6), "total": int32(10),
	})
}

// Some compatible upstreams name the event only in the SSE `event:` field and
// leave `type` out of the payload.
func TestSDKReadsResponsesEventNamedOnlyByTheSSEField(t *testing.T) {
	body := []byte("event: response.completed\n" +
		`data: {"response":{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n")

	assertUsage(t, ExtractUsage(body, "text/event-stream"), map[string]any{
		"prompt": int32(3), "completion": int32(2), "total": int32(5),
	})
}

// An early response event that carries a zeroed usage must not be mistaken for
// the final counts; an abandoned stream still needs its estimate.
func TestSDKDoesNotTreatEarlyResponsesUsageAsFinal(t *testing.T) {
	observation := &sseObservation{}
	observation.observeChunk([]byte(`data: {"type":"response.created","response":`+
		`{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`+"\n\n"),
		func() int32 { return 1 })
	observation.finish(func() int32 { return 1 })
	if observation.finalUsageSeen {
		t.Fatal("an early zeroed usage was treated as final")
	}

	completed := &sseObservation{}
	completed.observeChunk([]byte(`data: {"type":"response.completed","response":`+
		`{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n"),
		func() int32 { return 1 })
	completed.finish(func() int32 { return 1 })
	if !completed.finalUsageSeen {
		t.Fatal("the completed usage was not treated as final")
	}
}

// The legacy /v1/completions stream carries text rather than a delta; its
// first token is still what the console times.
func TestSDKSeesLegacyCompletionTextAsTheFirstToken(t *testing.T) {
	line := []byte(`data: {"object":"text_completion","choices":[{"text":"hi","index":0}]}` + "\n")
	if !HasVisibleToken(line) {
		t.Fatal("a legacy completion text chunk was not counted as the first token")
	}
	if HasVisibleToken([]byte(`data: {"object":"text_completion","choices":[{"text":"","index":0}]}` + "\n")) {
		t.Fatal("an empty legacy completion text was counted as the first token")
	}
}

// A chunk typed as Chat is not read by the Messages decoder, even when its
// usage is shaped like Anthropic's.
func TestSDKDoesNotReadAnthropicUsageFromAChatChunk(t *testing.T) {
	body := []byte(`data: {"type":"chat.completion.chunk","choices":[],` +
		`"usage":{"input_tokens":10,"output_tokens":5}}` + "\n\n")
	if usage := ExtractUsage(body, "text/event-stream"); usage.TotalTokens != nil {
		t.Fatalf("a Chat chunk read Anthropic usage %v", usage)
	}
}
