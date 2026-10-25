package proxy

import (
	"encoding/json"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"
)

// isOpenAIStreamEventType reports whether a payload's `type` names an OpenAI
// Chat or Responses stream event. The prefix test keeps an event added later
// on the SDK path instead of falling back to the generic walker.
func isOpenAIStreamEventType(name string) bool {
	return name == "chat.completion" ||
		strings.HasPrefix(name, "chat.completion.") ||
		strings.HasPrefix(name, "response.")
}

// isOpenAIChatChunk reports whether a payload is a Chat Completions chunk.
func isOpenAIChatChunk(eventType string) bool {
	return eventType == "chat.completion.chunk"
}

// decodeOpenAIStreamEvent dispatches an OpenAI stream event to the SDK parser
// that understands its shape.
func decodeOpenAIStreamEvent(data []byte, eventType string) openaiEvent {
	if isOpenAIChatChunk(eventType) {
		return decodeOpenAIChatEvent(data)
	}
	if strings.HasPrefix(eventType, "response.") {
		return decodeOpenAIResponseEvent(data)
	}
	return openaiEvent{matched: true}
}

// openaiUsageFields is the usage an OpenAI stream reported so far. Later
// events carry cumulative totals and replace the earlier ones.
type openaiUsageFields struct {
	prompt     *int32
	completion *int32
	total      *int32
	cached     *int32
	cacheWrite *int32
	reasoning  *int32
	seen       bool
}

func (f *openaiUsageFields) merge(next openaiUsageFields) {
	if !next.seen {
		return
	}
	f.seen = true
	if next.prompt != nil {
		f.prompt = next.prompt
	}
	if next.completion != nil {
		f.completion = next.completion
	}
	if next.total != nil {
		f.total = next.total
	}
	if next.cached != nil {
		f.cached = next.cached
	}
	if next.cacheWrite != nil {
		f.cacheWrite = next.cacheWrite
	}
	if next.reasoning != nil {
		f.reasoning = next.reasoning
	}
}

func (f openaiUsageFields) usage() TokenUsage {
	if !f.seen {
		return TokenUsage{}
	}
	total := f.total
	if total == nil {
		total = sumTokenParts(f.prompt, f.completion)
	}
	cacheCreation := f.cacheWrite
	return TokenUsage{
		PromptTokens:              f.prompt,
		CompletionTokens:          f.completion,
		TotalTokens:               total,
		PromptCachedTokens:        f.cached,
		CacheCreationTokens:       cacheCreation,
		CompletionReasoningTokens: f.reasoning,
	}
}

// openaiEvent is what one OpenAI stream event contributes.
type openaiEvent struct {
	// matched is false when the payload is not an OpenAI stream event.
	matched bool
	// visible reports whether the event carries the first visible token.
	visible bool
	usage   openaiUsageFields
	// finalUsage marks cumulative usage from an event that ends or finally
	// reports the counts, so an abandoned stream stops short of it.
	finalUsage bool
}

// decodeOpenAIChatEvent reads a Chat Completions chunk with the official SDK's
// types: the vendor defines delta and usage field names and their presence.
func decodeOpenAIChatEvent(data []byte) openaiEvent {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return openaiEvent{matched: true}
	}

	result := openaiEvent{matched: true}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" ||
			choice.Delta.Refusal != "" ||
			len(choice.Delta.ToolCalls) > 0 {
			result.visible = true
			break
		}
	}
	if chunk.JSON.Usage.Valid() {
		result.usage = usageFromCompletion(chunk.Usage)
		result.finalUsage = result.usage.seen
	}
	return result
}

// decodeOpenAIResponseEvent reads an OpenAI Responses stream event. Usage is
// read from the final response.completed; the visible-token judgement is left
// to the generic walker, whose Responses branch is separately tested.
func decodeOpenAIResponseEvent(data []byte) openaiEvent {
	var event responses.ResponseStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		return openaiEvent{matched: true}
	}
	result := openaiEvent{matched: true}
	if event.Type == "response.completed" && event.JSON.Response.Valid() {
		result.usage = usageFromResponse(event.Response.Usage)
		result.finalUsage = result.usage.seen
	}
	return result
}

func usageFromCompletion(usage openai.CompletionUsage) openaiUsageFields {
	fields := openaiUsageFields{}
	if usage.JSON.PromptTokens.Valid() {
		fields.prompt = usageInt64(usage.PromptTokens)
	}
	if usage.JSON.CompletionTokens.Valid() {
		fields.completion = usageInt64(usage.CompletionTokens)
	}
	if usage.JSON.TotalTokens.Valid() {
		fields.total = usageInt64(usage.TotalTokens)
	}
	if usage.PromptTokensDetails.JSON.CachedTokens.Valid() {
		fields.cached = usageInt64(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails.JSON.ReasoningTokens.Valid() {
		fields.reasoning = usageInt64(usage.CompletionTokensDetails.ReasoningTokens)
	}
	fields.seen = fields.prompt != nil || fields.completion != nil || fields.total != nil
	return fields
}

func usageFromResponse(usage responses.ResponseUsage) openaiUsageFields {
	fields := openaiUsageFields{}
	if usage.JSON.InputTokens.Valid() {
		fields.prompt = usageInt64(usage.InputTokens)
	}
	if usage.JSON.OutputTokens.Valid() {
		fields.completion = usageInt64(usage.OutputTokens)
	}
	if usage.JSON.TotalTokens.Valid() {
		fields.total = usageInt64(usage.TotalTokens)
	}
	if usage.InputTokensDetails.JSON.CachedTokens.Valid() {
		fields.cached = usageInt64(usage.InputTokensDetails.CachedTokens)
	}
	if usage.OutputTokensDetails.JSON.ReasoningTokens.Valid() {
		fields.reasoning = usageInt64(usage.OutputTokensDetails.ReasoningTokens)
	}
	fields.seen = fields.prompt != nil || fields.completion != nil || fields.total != nil
	return fields
}
