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
	if !message.Usage.JSON.CacheReadInputTokens.Valid() &&
		!message.Usage.JSON.CacheCreationInputTokens.Valid() {
		return TokenUsage{}, false
	}
	fields := usageFromAnthropic(message.Usage)
	if !fields.seen {
		return TokenUsage{}, false
	}
	return fields.usage(), true
}

// anthropicBodyUsage reads usage from a buffered Messages response.
func anthropicBodyUsage(raw []byte) (TokenUsage, bool) {
	var message anthropic.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		return TokenUsage{}, false
	}
	fields := usageFromAnthropic(message.Usage)
	if !fields.seen {
		return TokenUsage{}, false
	}
	return fields.usage(), true
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
	// A proxy-emitted top-level thinking_tokens is a breakdown, not a count to
	// add; it is read from the field the SDK keeps as unknown.
	if fields.thinking == nil {
		fields.thinking = extraAnthropicInt64(usage.JSON.ExtraFields, "thinking_tokens")
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
