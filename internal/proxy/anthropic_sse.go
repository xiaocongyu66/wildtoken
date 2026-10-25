package proxy

import (
	"encoding/json"
	"math"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// isAnthropicStreamEventType reports whether a payload's `type` names a
// Messages API stream event. The prefix test is deliberate: it covers the six
// documented events without listing each one, so an event added later is still
// read by the SDK rather than falling through to the generic walker.
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

// anthropicEvent is what one Messages stream event contributes.
type anthropicEvent struct {
	// matched is false when the payload is not a Messages stream event.
	matched bool
	// visible reports whether the event carries the first visible token.
	visible bool
	usage   anthropicUsageFields
	// finalUsage marks usage from an event other than message_start, whose
	// output count is a placeholder rather than the stream's total.
	finalUsage bool
}

// decodeAnthropicStreamEvent reads one Messages stream event with the official
// SDK's types, so the field names and their presence flags come from the
// provider rather than from this package.
func decodeAnthropicStreamEvent(data []byte, eventType string) anthropicEvent {
	if !isAnthropicStreamEventType(eventType) {
		return anthropicEvent{}
	}

	var event anthropic.MessageStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		return anthropicEvent{matched: true}
	}

	result := anthropicEvent{matched: true}
	switch eventType {
	case "message_start":
		if event.JSON.Message.Valid() {
			result.usage = usageFromAnthropic(event.Message.Usage)
		}
	case "message_delta":
		if event.JSON.Usage.Valid() {
			result.usage = usageFromDelta(event.Usage)
			result.finalUsage = result.usage.seen
		}
	case "content_block_delta":
		result.visible = event.Delta.Text != "" ||
			event.Delta.Thinking != "" ||
			event.Delta.PartialJSON != ""
	case "content_block_start":
		result.visible = event.ContentBlock.Text != "" || event.ContentBlock.Thinking != ""
	}
	return result
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
	fields.seen = fields.input != nil || fields.cacheRead != nil ||
		fields.cacheCreation != nil || fields.output != nil || fields.thinking != nil
	return fields
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
// as absent rather than clamped, for the reason usageInt32 documents.
func usageInt64(value int64) *int32 {
	if value < 0 || value > math.MaxInt32 {
		return nil
	}
	converted := int32(value)
	return &converted
}
