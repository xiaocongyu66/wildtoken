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
func decodeOpenAIChatEvent(data []byte, object string) (streamEvent, bool) {
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
		for _, name := range []string{"reasoning_content", "reasoning", "text"} {
			if text := extraString(delta.JSON.ExtraFields, name); text != "" {
				result.visible = true
				result.textDelta += estimateTextTokens(text)
			}
		}
	}
	// A standard chunk needs no shape fallback. A whole-message or legacy shape,
	// which the chunk type does not model, is tried; so is a payload labelled as
	// a chunk but carrying no delta at all.
	hasDelta := false
	for _, choice := range chunk.Choices {
		if choice.JSON.Delta.Valid() {
			hasDelta = true
			break
		}
	}
	if !result.visible && (object != "chat.completion.chunk" || !hasDelta) {
		// Some compatible upstreams stream whole messages rather than deltas.
		var message openai.ChatCompletion
		if err := json.Unmarshal(data, &message); err == nil {
			for _, choice := range message.Choices {
				if choice.Message.Content != "" || len(choice.Message.ToolCalls) > 0 {
					result.visible = true
					break
				}
			}
		}
		// The legacy /v1/completions stream carries text rather than deltas.
		var legacy openai.Completion
		if err := json.Unmarshal(data, &legacy); err == nil {
			for _, choice := range legacy.Choices {
				if choice.Text != "" {
					result.visible = true
					result.textDelta += estimateTextTokens(choice.Text)
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
func decodeOpenAIResponseEvent(data []byte, eventType string) streamEvent {
	var event responses.ResponseStreamEventUnion
	if err := json.Unmarshal(data, &event); err != nil {
		return streamEvent{matched: true}
	}

	result := streamEvent{matched: true}
	switch eventType {
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

	switch eventType {
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
	}
	if eventType == "response.failed" {
		// A failed event is a failure even when it carries no error object; a
		// stream that began as a 200 must not be logged as a success.
		//
		// The SDK's ResponseError has a code but no type, so the error is
		// decoded once more to keep a type-only fault, which is what the channel
		// fault list keys on.
		var failure openai.ErrorObject
		if raw := event.JSON.Response.Raw(); raw != "" {
			var envelope struct {
				Error openai.ErrorObject `json:"error"`
			}
			_ = json.Unmarshal([]byte(raw), &envelope)
			failure = envelope.Error
		}
		kind := failure.Code
		if kind == "" {
			kind = failure.Type
		}
		result.hasError = true
		result.errKind = kind
		result.errDetail = failure.Message
		result.fault = channelFaultStreamErrors[kind]
		// A failed response may still report what it spent before failing.
		result.openai = usageFromResponse(event.Response.Usage)
		result.finalUsage = result.openai.seen
	} else if event.JSON.Response.Valid() {
		result.openai = usageFromResponse(event.Response.Usage)
		// Only a terminal response's usage is final. An early event may carry
		// a zeroed usage, and treating it as final would skip the estimate for
		// a stream abandoned before the real counts arrived.
		switch eventType {
		case "response.completed", "response.incomplete", "response.cancelled":
			result.finalUsage = result.openai.seen
		}
	}
	// Some compatible upstreams put usage at the top level of the event rather
	// than under response. Read it whenever the response object did not carry
	// it; only a terminal event's top-level usage is final.
	if !result.openai.seen {
		var wrapper struct {
			Usage responses.ResponseUsage `json:"usage"`
		}
		if err := json.Unmarshal(data, &wrapper); err == nil {
			if fields := usageFromResponse(wrapper.Usage); fields.seen {
				result.openai = fields
				switch eventType {
				case "response.completed", "response.incomplete", "response.cancelled":
					result.finalUsage = true
				}
			}
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
	for _, response := range responseCandidates(raw) {
		if !response.Usage.JSON.InputTokens.Valid() || !response.Usage.JSON.TotalTokens.Valid() {
			continue
		}
		return usageFromResponse(response.Usage).usage(), true
	}
	return TokenUsage{}, false
}

// responseCandidates decodes a Responses body both as itself and as the
// `{"response": ...}` wrapper some gateways add.
func responseCandidates(raw []byte) []responses.Response {
	var response responses.Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil
	}
	candidates := []responses.Response{response}
	var wrapped struct {
		Response responses.Response `json:"response"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil {
		candidates = append(candidates, wrapped.Response)
	}
	return candidates
}

// responseReasoningEffort reads the effort a buffered response reported.
func responseReasoningEffort(raw []byte) (string, bool) {
	for _, response := range responseCandidates(raw) {
		if effort, ok := effortFromResponse(response); ok {
			return effort, true
		}
	}
	return "", false
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
		// prompt_tokens is what makes this a Chat usage. A Responses or
		// Anthropic usage also has a total, and would otherwise be returned
		// with its prompt and completion missing.
		if fields := usageFromCompletion(completion); fields.prompt != nil {
			return fields, true
		}
	}
	var response responses.ResponseUsage
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&response); err == nil {
		// A Responses usage always reports a total; an Anthropic usage never
		// does, and must fall through to the Messages reader below. A usage
		// that carries Anthropic's cache counters is Anthropic even when a
		// gateway added a total to it.
		if fields := usageFromResponse(response); fields.seen && fields.total != nil &&
			!looksAnthropicUsage(raw) {
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
	if fields.cached == nil || fields.cacheWrite == nil {
		// Some gateways use input_tokens_details (or input_token_details)
		// instead of prompt_tokens_details; the SDK keeps both as unknown.
		for _, name := range []string{"input_tokens_details", "input_token_details"} {
			details := nestedTokenDetails(usage.JSON.ExtraFields, name)
			if fields.cached == nil {
				fields.cached = usageInt32Value(details.CachedTokens)
				if fields.cached == nil {
					fields.cached = usageInt32Value(details.CacheRead)
				}
			}
			if fields.cacheWrite == nil {
				fields.cacheWrite = usageInt32Value(details.CacheWrite)
				if fields.cacheWrite == nil {
					fields.cacheWrite = usageInt32Value(details.CacheCreation)
				}
			}
		}
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
	if fields.reasoning == nil {
		fields.reasoning = firstExtraUsageInt64(usage.JSON.ExtraFields, "thinking_tokens")
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
	if fields.cached == nil || fields.cacheWrite == nil {
		details := nestedTokenDetails(usage.JSON.ExtraFields, "input_token_details")
		if fields.cached == nil {
			fields.cached = usageInt32Value(details.CacheRead)
		}
		if fields.cacheWrite == nil {
			fields.cacheWrite = usageInt32Value(details.CacheCreation)
		}
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
