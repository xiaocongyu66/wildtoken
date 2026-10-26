package proxy

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/respjson"
)

// isAnthropicStreamEventType reports whether a payload's `type` names a
// Messages API stream event. The prefix test covers the documented events
// without listing each one, so an event added later is still read by the SDK.
func isAnthropicStreamEventType(name string) bool {
	return strings.HasPrefix(name, "message_") || strings.HasPrefix(name, "content_block_")
}

// anthropicUsageFields is the usage a Messages stream reported so far.
//
// Anthropic's input is residual: the total prompt is input_tokens plus the two
// cache counters. Later events repeat the cumulative figures, so a field a
// later event carries replaces the earlier one.
type anthropicUsageFields struct {
	input         *int32
	cacheRead     *int32
	cacheCreation *int32
	output        *int32
	thinking      *int32
	seen          bool
}

func (f *anthropicUsageFields) merge(next anthropicUsageFields) {
	if !next.seen {
		return
	}
	f.seen = true
	if next.input != nil {
		f.input = next.input
	}
	if next.cacheRead != nil {
		f.cacheRead = next.cacheRead
	}
	if next.cacheCreation != nil {
		f.cacheCreation = next.cacheCreation
	}
	if next.output != nil {
		f.output = next.output
	}
	if next.thinking != nil {
		f.thinking = next.thinking
	}
}

func (f anthropicUsageFields) usage() TokenUsage {
	if !f.seen {
		return TokenUsage{}
	}
	prompt := f.input
	if aggregated := sumTokenParts(f.input, f.cacheCreation, f.cacheRead); aggregated != nil {
		prompt = aggregated
	}
	return TokenUsage{
		PromptTokens:              prompt,
		CompletionTokens:          f.output,
		TotalTokens:               sumTokenParts(prompt, f.output),
		PromptCachedTokens:        f.cacheRead,
		CacheCreationTokens:       f.cacheCreation,
		CompletionReasoningTokens: f.thinking,
	}
}

// decodeAnthropicStreamEvent reads one Messages stream event with the official
// SDK's types.
func decodeAnthropicStreamEvent(data []byte, eventType string) streamEvent {
	var event anthropic.MessageStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		return streamEvent{matched: true}
	}

	result := streamEvent{matched: true}
	switch eventType {
	case "message_start":
		if event.JSON.Message.Valid() {
			result.anthropic = usageFromAnthropic(event.Message.Usage)
		}
	case "message_delta":
		if event.JSON.Usage.Valid() {
			result.anthropic = usageFromDelta(event.Usage)
			result.finalUsage = result.anthropic.seen
		}
	case "content_block_delta":
		result.visible = event.Delta.Text != "" ||
			event.Delta.Thinking != "" ||
			event.Delta.PartialJSON != ""
		result.textDelta = estimateTextTokens(event.Delta.Text) +
			estimateTextTokens(event.Delta.Thinking) +
			estimateTextTokens(event.Delta.PartialJSON)
	case "content_block_start":
		result.visible = event.ContentBlock.Text != "" || event.ContentBlock.Thinking != ""
	case "message_stop":
		result.terminal = true
	}
	// Some compatible upstreams put usage at the top level of any event rather
	// than under message_start's message. It is read whenever the standard
	// shape did not provide it; only message_start's is not final.
	if !result.anthropic.seen && event.JSON.Usage.Valid() {
		result.anthropic = usageFromDelta(event.Usage)
		result.finalUsage = result.anthropic.seen && eventType != "message_start"
	}
	return result
}

// anthropicCacheUsage reports usage only when the body names Anthropic's
// top-level cache counters. A gateway may add total_tokens to a Messages
// usage, which would otherwise let the Responses reader claim the body and
// drop the cache from the prompt.
func anthropicCacheUsage(raw []byte) (TokenUsage, bool) {
	var message anthropic.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		return TokenUsage{}, false
	}
	if !anthropicUsageClassified(message.Usage) {
		return TokenUsage{}, false
	}
	fields := usageFromAnthropic(message.Usage)
	if !fields.seen {
		return TokenUsage{}, false
	}
	return fields.usage(), true
}

// anthropicUsageClassified reports whether a usage object is Anthropic-shaped:
// a top-level cache read or a nested thinking breakdown marks it outright, a
// bare cache_creation only when no total is reported. With a total and no cache
// read, the cache counters are subsets and are not added.
func anthropicUsageClassified(usage anthropic.Usage) bool {
	hasCacheRead := usage.JSON.CacheReadInputTokens.Valid() ||
		extraAnthropicInt64(usage.JSON.ExtraFields, "cache_read_tokens") != nil
	hasThinking := usage.OutputTokensDetails.JSON.ThinkingTokens.Valid()
	hasTotal := extraAnthropicInt64(usage.JSON.ExtraFields, "total_tokens") != nil
	hasCacheCreationOnly := usage.JSON.CacheCreationInputTokens.Valid() && !hasTotal
	return hasCacheRead || hasThinking || hasCacheCreationOnly
}

// looksAnthropicUsage reports whether a bare usage object is Anthropic-shaped.
//
// raw may be followed by the rest of an oversized event, so it is decoded with
// a streaming decoder that stops at the first value.
func looksAnthropicUsage(raw []byte) bool {
	var usage anthropic.Usage
	return json.NewDecoder(bytes.NewReader(raw)).Decode(&usage) == nil &&
		anthropicUsageClassified(usage)
}

// anthropicBodyUsage reads usage from a buffered Messages response, including
// the `message` wrapper some gateways add.
func anthropicBodyUsage(raw []byte) (TokenUsage, bool) {
	var message anthropic.Message
	if err := json.Unmarshal(raw, &message); err == nil {
		if fields := usageFromAnthropic(message.Usage); fields.seen {
			return fields.usage(), true
		}
	}
	var wrapped struct {
		Message anthropic.Message `json:"message"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		if fields := usageFromAnthropic(wrapped.Message.Usage); fields.seen {
			return fields.usage(), true
		}
	}
	return TokenUsage{}, false
}

// anthropicUsageFromJSON reads a bare usage object, as found at the end of an
// event too large to buffer whole.
func anthropicUsageFromJSON(raw []byte) (anthropicUsageFields, bool) {
	var usage anthropic.Usage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&usage); err != nil {
		return anthropicUsageFields{}, false
	}
	fields := usageFromAnthropic(usage)
	return fields, fields.seen
}

func usageFromAnthropic(usage anthropic.Usage) anthropicUsageFields {
	fields := anthropicUsageFields{}
	if usage.JSON.InputTokens.Valid() {
		fields.input = usageInt64(usage.InputTokens)
	}
	if usage.JSON.CacheReadInputTokens.Valid() {
		fields.cacheRead = usageInt64(usage.CacheReadInputTokens)
	}
	if usage.JSON.CacheCreationInputTokens.Valid() {
		fields.cacheCreation = usageInt64(usage.CacheCreationInputTokens)
	}
	if usage.JSON.OutputTokens.Valid() {
		fields.output = usageInt64(usage.OutputTokens)
	}
	if usage.OutputTokensDetails.JSON.ThinkingTokens.Valid() {
		fields.thinking = usageInt64(usage.OutputTokensDetails.ThinkingTokens)
	}
	// A proxy-emitted top-level thinking_tokens or a reasoning_tokens breakdown
	// is a detail, not a count to add; they are read from the fields the SDK
	// keeps as unknown.
	if fields.thinking == nil {
		fields.thinking = extraAnthropicInt64(usage.JSON.ExtraFields, "thinking_tokens")
	}
	if fields.thinking == nil {
		fields.thinking = extraAnthropicInt64(usage.OutputTokensDetails.JSON.ExtraFields,
			"reasoning_tokens")
	}
	if fields.cacheRead == nil || fields.cacheCreation == nil {
		details := nestedTokenDetails(usage.JSON.ExtraFields, "input_token_details")
		if fields.cacheRead == nil {
			fields.cacheRead = usageInt32Value(details.CacheRead)
		}
		if fields.cacheCreation == nil {
			fields.cacheCreation = usageInt32Value(details.CacheCreation)
		}
	}
	if fields.cacheRead == nil {
		fields.cacheRead = extraAnthropicInt64(usage.JSON.ExtraFields, "cache_read_tokens")
	}
	if fields.cacheCreation == nil {
		fields.cacheCreation = extraAnthropicInt64(usage.JSON.ExtraFields,
			"cache_creation_tokens")
	}
	fields.seen = fields.input != nil || fields.cacheRead != nil ||
		fields.cacheCreation != nil || fields.output != nil || fields.thinking != nil
	return fields
}

// extraAnthropicInt64 reads an unknown Messages usage counter.
func extraAnthropicInt64(fields map[string]respjson.Field, name string) *int32 {
	field, present := fields[name]
	if !present {
		return nil
	}
	raw := field.Raw()
	if raw == "" || raw == respjson.Null {
		return nil
	}
	var value int64
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil
	}
	return usageInt64(value)
}

func usageFromDelta(usage anthropic.MessageDeltaUsage) anthropicUsageFields {
	fields := anthropicUsageFields{}
	if usage.JSON.InputTokens.Valid() {
		fields.input = usageInt64(usage.InputTokens)
	}
	if usage.JSON.CacheReadInputTokens.Valid() {
		fields.cacheRead = usageInt64(usage.CacheReadInputTokens)
	}
	if usage.JSON.CacheCreationInputTokens.Valid() {
		fields.cacheCreation = usageInt64(usage.CacheCreationInputTokens)
	}
	if usage.JSON.OutputTokens.Valid() {
		fields.output = usageInt64(usage.OutputTokens)
	}
	if usage.OutputTokensDetails.JSON.ThinkingTokens.Valid() {
		fields.thinking = usageInt64(usage.OutputTokensDetails.ThinkingTokens)
	}
	if fields.thinking == nil {
		fields.thinking = extraAnthropicInt64(usage.JSON.ExtraFields, "thinking_tokens")
	}
	if fields.thinking == nil {
		fields.thinking = extraAnthropicInt64(usage.OutputTokensDetails.JSON.ExtraFields,
			"reasoning_tokens")
	}
	if fields.cacheRead == nil || fields.cacheCreation == nil {
		details := nestedTokenDetails(usage.JSON.ExtraFields, "input_token_details")
		if fields.cacheRead == nil {
			fields.cacheRead = usageInt32Value(details.CacheRead)
		}
		if fields.cacheCreation == nil {
			fields.cacheCreation = usageInt32Value(details.CacheCreation)
		}
	}
	if fields.cacheRead == nil {
		fields.cacheRead = extraAnthropicInt64(usage.JSON.ExtraFields, "cache_read_tokens")
	}
	if fields.cacheCreation == nil {
		fields.cacheCreation = extraAnthropicInt64(usage.JSON.ExtraFields,
			"cache_creation_tokens")
	}
	fields.seen = fields.input != nil || fields.cacheRead != nil ||
		fields.cacheCreation != nil || fields.output != nil || fields.thinking != nil
	return fields
}

// usageInt64 narrows a provider count. A figure that will not fit is reported
// as absent rather than clamped: one malformed report must not spend a token's
// entire budget.
func usageInt64(value int64) *int32 {
	if value < 0 || value > math.MaxInt32 {
		return nil
	}
	converted := int32(value)
	return &converted
}
