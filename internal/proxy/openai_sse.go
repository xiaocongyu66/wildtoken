package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/respjson"
	"github.com/openai/openai-go/responses"
)

// channelFaultStreamErrors are the in-stream error kinds that blame the
// channel rather than the request: overload, server faults and rate limits.
var channelFaultStreamErrors = map[string]bool{
	"overloaded_error":    true,
	"api_error":           true,
	"rate_limit_error":    true,
	"server_error":        true,
	"rate_limit_exceeded": true,
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
	return TokenUsage{
		PromptTokens:              f.prompt,
		CompletionTokens:          f.completion,
		TotalTokens:               total,
		PromptCachedTokens:        f.cached,
		CacheCreationTokens:       f.cacheWrite,
		CompletionReasoningTokens: f.reasoning,
	}
}

// decodeOpenAIChatEvent reads a Chat Completions chunk with the official SDK.
func decodeOpenAIChatEvent(data []byte) (streamEvent, bool) {
	var chunk openai.ChatCompletionChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return streamEvent{}, false
	}
	matched := chunk.JSON.Object.Valid() || len(chunk.Choices) > 0 || chunk.JSON.Usage.Valid()
	if !matched {
		return streamEvent{}, false
	}

	result := streamEvent{matched: true}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta.Content != "" ||
			delta.Refusal != "" ||
			delta.FunctionCall.Name != "" ||
			delta.FunctionCall.Arguments != "" ||
			len(delta.ToolCalls) > 0 {
			result.visible = true
		}
		result.textDelta += estimateTextTokens(delta.Content)
		result.textDelta += estimateTextTokens(delta.FunctionCall.Name)
		result.textDelta += estimateTextTokens(delta.FunctionCall.Arguments)
		for _, call := range delta.ToolCalls {
			result.textDelta += estimateTextTokens(call.Function.Name)
			result.textDelta += estimateTextTokens(call.Function.Arguments)
		}
		// Reasoning text has no field in the SDK's chunk type; vendors put it in
		// the payload the SDK keeps as extra fields.
		for _, name := range []string{"reasoning_content", "reasoning"} {
			if text := extraString(delta.JSON.ExtraFields, name); text != "" {
				result.visible = true
				result.textDelta += estimateTextTokens(text)
			}
		}
	}
	// Some compatible upstreams stream whole messages rather than deltas.
	if !result.visible {
		var completion openai.ChatCompletion
		if err := json.Unmarshal(data, &completion); err == nil {
			for _, choice := range completion.Choices {
				if choice.Message.Content != "" || len(choice.Message.ToolCalls) > 0 {
					result.visible = true
					break
				}
			}
		}
	}
	if chunk.JSON.Usage.Valid() {
		result.openai = usageFromCompletion(chunk.Usage)
		result.finalUsage = result.openai.seen
	}
	return result, true
}

// decodeOpenAIResponseEvent reads an OpenAI Responses stream event.
func decodeOpenAIResponseEvent(data []byte) streamEvent {
	var event responses.ResponseStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		return streamEvent{matched: true}
	}

	result := streamEvent{matched: true}
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled":
		result.terminal = true
	case "error":
		// The Responses API reports an error event's fields at the top level.
		result.terminal = true
		result.hasError = true
		result.errKind = event.Code
		result.errDetail = event.Message
		result.fault = channelFaultStreamErrors[event.Code]
	}

	switch event.Type {
	case "response.output_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		result.visible = event.Delta.OfString != "" ||
			event.Delta.OfResponseReasoningSummaryDeltaEventDelta != nil
		result.textDelta = estimateTextTokens(event.Delta.OfString)
	}

	if event.JSON.Response.Valid() {
		if effort, ok := effortFromResponse(event.Response); ok {
			result.effort, result.hasEffort = effort, true
		}
		if event.Type == "response.failed" {
			failure := event.Response.Error
			result.hasError = true
			result.errKind = string(failure.Code)
			result.errDetail = failure.Message
			result.fault = channelFaultStreamErrors[result.errKind]
		} else {
			result.openai = usageFromResponse(event.Response.Usage)
			result.finalUsage = result.openai.seen
		}
	}
	return result
}

// decodeNestedErrorEvent reads an error object nested under `error`, which is
// how Anthropic reports a stream failure and how an OpenAI-compatible upstream
// sends a bare error chunk.
func decodeNestedErrorEvent(data []byte) (streamEvent, bool) {
	var envelope struct {
		Error *openai.ErrorObject `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Error == nil {
		return streamEvent{}, false
	}
	kind := envelope.Error.Code
	if kind == "" {
		kind = envelope.Error.Type
	}
	return streamEvent{
		matched:   true,
		terminal:  true,
		hasError:  true,
		errKind:   kind,
		errDetail: envelope.Error.Message,
		fault:     channelFaultStreamErrors[kind],
	}, true
}

// chatBodyUsage reads usage from a buffered Chat Completions response.
//
// prompt_tokens is what distinguishes this shape: Responses and Anthropic both
// count input_tokens, and a Responses body's total_tokens would otherwise make
// a Chat decode look successful.
func chatBodyUsage(raw []byte) (TokenUsage, bool) {
	var completion openai.ChatCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return TokenUsage{}, false
	}
	if !completion.Usage.JSON.PromptTokens.Valid() {
		return TokenUsage{}, false
	}
	return usageFromCompletion(completion.Usage).usage(), true
}

// responseBodyUsage reads usage from a buffered Responses response.
//
// A Responses usage always reports a total, which an Anthropic usage never
// does; requiring both keeps Anthropic bodies out of this branch.
func responseBodyUsage(raw []byte) (TokenUsage, bool) {
	var response responses.Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return TokenUsage{}, false
	}
	if !response.Usage.JSON.InputTokens.Valid() || !response.Usage.JSON.TotalTokens.Valid() {
		return TokenUsage{}, false
	}
	return usageFromResponse(response.Usage).usage(), true
}

// responseReasoningEffort reads the effort a buffered response reported.
func responseReasoningEffort(raw []byte) (string, bool) {
	var response responses.Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", false
	}
	return effortFromResponse(response)
}

func effortFromResponse(response responses.Response) (string, bool) {
	if !response.Reasoning.JSON.Effort.Valid() {
		return "", false
	}
	effort := strings.TrimSpace(string(response.Reasoning.Effort))
	return effort, effort != ""
}

// openaiUsageFromJSON reads a bare usage object, as found at the end of an
// event too large to buffer whole.
func openaiUsageFromJSON(raw []byte) (openaiUsageFields, bool) {
	var completion openai.CompletionUsage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&completion); err == nil {
		if fields := usageFromCompletion(completion); fields.seen {
			return fields, true
		}
	}
	var response responses.ResponseUsage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&response); err == nil {
		// A Responses usage always reports a total; an Anthropic usage never
		// does, and must fall through to the Messages reader below.
		if fields := usageFromResponse(response); fields.seen && fields.total != nil {
			return fields, true
		}
	}
	return openaiUsageFields{}, false
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
	fields.cacheWrite = firstExtraUsageInt64(usage.JSON.ExtraFields,
		"cache_creation_input_tokens", "cache_creation_tokens", "cache_write_tokens")
	if fields.cacheWrite == nil {
		fields.cacheWrite = firstExtraUsageInt64(usage.PromptTokensDetails.JSON.ExtraFields,
			"cache_write_tokens", "cache_creation_tokens", "cache_creation")
	}
	if fields.cached == nil {
		fields.cached = firstExtraUsageInt64(usage.JSON.ExtraFields,
			"cache_read_input_tokens", "cache_read_tokens")
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
	fields.cacheWrite = firstExtraUsageInt64(usage.JSON.ExtraFields,
		"cache_creation_input_tokens", "cache_creation_tokens", "cache_write_tokens")
	if fields.cacheWrite == nil {
		fields.cacheWrite = firstExtraUsageInt64(usage.InputTokensDetails.JSON.ExtraFields,
			"cache_write_tokens", "cache_creation_tokens", "cache_creation")
	}
	if fields.cached == nil {
		fields.cached = firstExtraUsageInt64(usage.JSON.ExtraFields,
			"cache_read_input_tokens", "cache_read_tokens")
	}
	fields.seen = fields.prompt != nil || fields.completion != nil || fields.total != nil
	return fields
}

// firstExtraUsageInt64 reads the first named counter the SDK kept as an
// unknown usage field.
//
// The SDK records an untyped extra with an "invalid" status but keeps its raw
// JSON, so presence is tested on Raw rather than Valid.
func firstExtraUsageInt64(fields map[string]respjson.Field, names ...string) *int32 {
	for _, name := range names {
		field, present := fields[name]
		if !present {
			continue
		}
		raw := field.Raw()
		if raw == "" || raw == respjson.Null {
			continue
		}
		var value int64
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			continue
		}
		if converted := usageInt64(value); converted != nil {
			return converted
		}
	}
	return nil
}

// extraString reads a string the SDK kept as an unknown field.
func extraString(fields map[string]respjson.Field, name string) string {
	field, present := fields[name]
	if !present {
		return ""
	}
	raw := field.Raw()
	if raw == "" || raw == respjson.Null {
		return ""
	}
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}
	return value
}
