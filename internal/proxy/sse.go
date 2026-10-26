package proxy

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
)

// maxSSEEventBytes bounds one buffered SSE line, so a malformed stream that
// never emits a newline cannot grow the observer without end.
const maxSSEEventBytes = 4 * 1024 * 1024

// TokenUsage is the accounting a response reported.
type TokenUsage struct {
	PromptTokens              *int32
	CompletionTokens          *int32
	TotalTokens               *int32
	PromptCachedTokens        *int32
	CacheCreationTokens       *int32
	CompletionReasoningTokens *int32
}

// streamEvent is what one stream payload contributed, in vendor-neutral terms.
//
// The vendor decoders fill in only the parts their SDK models; the observer
// merges everything that arrives, because a stream may split its report across
// events.
type streamEvent struct {
	matched    bool
	visible    bool
	terminal   bool
	finalUsage bool
	textDelta  int64
	effort     string
	hasEffort  bool
	errKind    string
	errDetail  string
	hasError   bool
	fault      bool

	anthropic anthropicUsageFields
	openai    openaiUsageFields
}

// errorMessage renders a stream failure the way the request log records it.
func (e streamEvent) errorMessage() string {
	message := "upstream reported an error in the stream"
	for _, part := range []string{e.errKind, e.errDetail} {
		if part != "" {
			message += ": " + part
		}
	}
	return message
}

// eventEnvelope peeks at the two fields that say which protocol a payload is,
// so the right SDK type decodes it.
type eventEnvelope struct {
	Type   string `json:"type"`
	Object string `json:"object"`
}

// decodeStreamEvent reads one SSE payload with the vendor SDK that owns it.
//
// hint is the SSE `event:` name of the frame the payload arrived in. Some
// compatible upstreams type their events only there, leaving the data without
// a `type` field, and the name is what tells the decoders what they are
// looking at.
func decodeStreamEvent(data []byte, hint string) streamEvent {
	var envelope eventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return streamEvent{}
	}
	if envelope.Type == "" {
		envelope.Type = hint
	}
	switch {
	case isAnthropicStreamEventType(envelope.Type):
		return decodeAnthropicStreamEvent(data, envelope.Type)
	case envelope.Type == "error":
		// Anthropic nests the failure under `error`; the Responses API puts it
		// at the top level of the event.
		if event, ok := decodeNestedErrorEvent(data); ok {
			return event
		}
		return decodeOpenAIResponseEvent(data, envelope.Type)
	case strings.HasPrefix(envelope.Type, "response."):
		return decodeOpenAIResponseEvent(data, envelope.Type)
	default:
		if event, ok := decodeOpenAIChatEvent(data); ok {
			return event
		}
		if event, ok := decodeNestedErrorEvent(data); ok {
			return event
		}
		return streamEvent{}
	}
}

// sumTokenParts adds the parts that are present, or returns nil when none are.
//
// The sum is taken in int64 because three upstream-supplied counts can each be
// in range and still overflow int32 together, and a total that wrapped negative
// reads as "no usage" to the quota counter. A total that does not fit is
// reported absent, for the same reason usageInt64 does.
func sumTokenParts(parts ...*int32) *int32 {
	var sum int64
	any := false
	for _, part := range parts {
		if part != nil {
			any = true
			sum += int64(*part)
		}
	}
	if !any || sum > math.MaxInt32 {
		return nil
	}
	total := int32(sum)
	return &total
}

// forEachSSELine walks a buffered body one newline-delimited line at a time,
// stopping early when visit returns false.
//
// The lines are slices of the body rather than copies. Converting the body to a
// string and splitting it allocated a second copy of the whole thing plus a
// slice header for every line it contained — several times the body's own size
// before a single byte had been parsed.
func forEachSSELine(body []byte, visit func(line []byte) bool) {
	for len(body) > 0 {
		line, rest, found := bytes.Cut(body, []byte("\n"))
		if !visit(line) {
			return
		}
		if !found {
			return
		}
		body = rest
	}
}

// sseDataBytes returns the payload of a `data:` line, as a slice of the line.
func sseDataBytes(line []byte) ([]byte, bool) {
	rest, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !found {
		return nil, false
	}
	return bytes.TrimLeft(rest, " \t"), true
}

// sseData returns the payload of a `data:` line.
func sseData(line string) (string, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), "data:")
	if !found {
		return "", false
	}
	return strings.TrimLeft(rest, " \t"), true
}

// HasVisibleToken reports whether a buffered SSE chunk contains a content token.
func HasVisibleToken(chunk []byte) bool {
	visible := false
	eventName := ""
	forEachSSELine(chunk, func(line []byte) bool {
		eventName = sseFrameName(line, eventName)
		data, ok := sseDataBytes(line)
		if !ok {
			return true
		}
		if len(data) == 0 || string(data) == "[DONE]" {
			return true
		}
		if decodeStreamEvent(data, eventName).visible {
			visible = true
			return false
		}
		return true
	})
	return visible
}

// ExtractUsage reads token usage from either an SSE stream body or a JSON body.
func ExtractUsage(rawBody []byte, contentType string) TokenUsage {
	if IsSSEContentType(contentType) || strings.Contains(strings.ToLower(contentType), "sse") {
		// A stream reports usage repeatedly, sometimes split across events.
		var observation sseObservation
		var eventName string
		forEachSSELine(rawBody, func(line []byte) bool {
			eventName = sseFrameName(line, eventName)
			data, ok := sseDataBytes(line)
			if !ok {
				return true
			}
			if len(data) == 0 || string(data) == "[DONE]" {
				return true
			}
			event := decodeStreamEvent(data, eventName)
			if !event.matched {
				return true
			}
			observation.anthropicUsage.merge(event.anthropic)
			observation.openaiUsage.merge(event.openai)
			return true
		})
		return observation.tokenUsage()
	}

	return extractBodyUsage(rawBody)
}

// extractBodyUsage reads usage from a buffered body by letting each vendor's
// SDK decode it; the first one to report a count owns the body.
func extractBodyUsage(raw []byte) TokenUsage {
	if usage, ok := chatBodyUsage(raw); ok {
		return usage
	}
	if usage, ok := anthropicCacheUsage(raw); ok {
		return usage
	}
	if usage, ok := responseBodyUsage(raw); ok {
		return usage
	}
	if usage, ok := anthropicBodyUsage(raw); ok {
		return usage
	}
	return TokenUsage{}
}

// IsSSEContentType reports whether a content type marks a server-sent stream.
func IsSSEContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "event-stream")
}

func isTerminalSSEEventType(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete",
		"response.cancelled", "message_stop", "error":
		return true
	default:
		return false
	}
}

// sseBytesLineIsTerminal reports whether a line ends the stream, either as an
// `event:` name or as the payload's own type.
func sseBytesLineIsTerminal(line []byte) bool {
	text := strings.TrimSpace(string(bytes.TrimSuffix(line, []byte("\r"))))
	if lineTerminal(text) {
		return true
	}
	data, found := sseData(text)
	if !found || data == "" || data == "[DONE]" {
		return false
	}
	return decodeStreamEvent([]byte(data), "").terminal
}

// sseEventName returns the name of an `event:` line.
func sseEventName(line []byte) (string, bool) {
	name, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("event:"))
	if !found {
		return "", false
	}
	return strings.TrimSpace(string(name)), true
}

// sseFrameName tracks the `event:` name of the frame a line belongs to. A
// blank line dispatches the frame and clears the name, so it cannot leak into
// the next one.
func sseFrameName(line []byte, current string) string {
	if len(bytes.TrimSpace(line)) == 0 {
		return ""
	}
	if name, found := sseEventName(line); found {
		return name
	}
	return current
}

// lineTerminal reports a terminal `event:` name or `[DONE]` without a payload.
func lineTerminal(text string) bool {
	if name, found := strings.CutPrefix(text, "event:"); found {
		return isTerminalSSEEventType(strings.TrimSpace(name))
	}
	data, found := sseData(text)
	return found && data == "[DONE]"
}

// sseObservation accumulates what a stream reveals as its bytes pass through.
type sseObservation struct {
	lineBuf              []byte
	lineOverflow         bool
	firstTokenMs         *int32
	terminalEventPending bool
	terminalEventSeen    bool
	// eventName is the `event:` name of the frame being read, for upstreams
	// that type their events only there.
	eventName               string
	anthropicUsage          anthropicUsageFields
	openaiUsage             openaiUsageFields
	responseReasoningEffort *string
	// streamError is the failure an error event reported, nil for a stream
	// that ended normally. streamErrorFault says whether it blames the channel.
	streamError      *string
	streamErrorFault bool
	// overflowTail keeps the end of a line too long to buffer, where an
	// oversized event carries its usage.
	overflowTail []byte
	// finalUsageSeen is set by usage from any event but message_start, whose
	// output count is a placeholder. outputEstimate approximates the tokens the
	// stream's deltas carried, for billing one abandoned before its usage.
	finalUsageSeen bool
	outputEstimate int64
}

// tokenUsage is the usage the stream reported so far.
func (o *sseObservation) tokenUsage() TokenUsage {
	if o.anthropicUsage.seen {
		return o.anthropicUsage.usage()
	}
	return o.openaiUsage.usage()
}

// observeLine folds one complete line into the observation. elapsedMs reports
// how long the request has been running, for time-to-first-token.
func (o *sseObservation) observeLine(line []byte, elapsedMs func() int32) {
	text := strings.TrimSpace(string(bytes.TrimSuffix(line, []byte("\r"))))
	if text == "" {
		// A blank line closes an event, which is what confirms a pending
		// terminal event actually arrived in full.
		if o.terminalEventPending {
			o.terminalEventSeen = true
		}
		o.terminalEventPending = false
		o.eventName = ""
		return
	}

	data, isData := sseData(text)
	var event streamEvent
	if isData {
		if data != "" && data != "[DONE]" {
			event = decodeStreamEvent([]byte(data), o.eventName)
		}
	} else if name, found := sseEventName(line); found {
		o.eventName = name
	}

	if o.firstTokenMs == nil && event.visible {
		measured := elapsedMs()
		o.firstTokenMs = &measured
	}
	if !o.terminalEventSeen && (event.terminal || lineTerminal(text)) {
		o.terminalEventPending = true
	}
	if !event.matched {
		return
	}

	o.anthropicUsage.merge(event.anthropic)
	o.openaiUsage.merge(event.openai)
	if event.finalUsage {
		o.finalUsageSeen = true
	}
	o.outputEstimate += event.textDelta
	if o.streamError == nil && event.hasError {
		message := event.errorMessage()
		o.streamError = &message
		o.streamErrorFault = event.fault
	}
	if o.responseReasoningEffort == nil && event.hasEffort {
		effort := event.effort
		o.responseReasoningEffort = &effort
	}
}

// observeChunk splits a chunk into lines, buffering an incomplete tail.
func (o *sseObservation) observeChunk(chunk []byte, elapsedMs func() int32) {
	for len(chunk) > 0 {
		newlineIndex := bytes.IndexByte(chunk, '\n')
		var segment []byte
		completeLine := newlineIndex >= 0
		if completeLine {
			segment = chunk[:newlineIndex]
			chunk = chunk[newlineIndex+1:]
		} else {
			segment = chunk
			chunk = nil
		}

		// An oversized line is discarded rather than buffered, and the discard
		// continues until its terminating newline arrives. Only its tail is
		// kept, for the usage.
		switch {
		case o.lineOverflow:
			o.keepOverflowTail(segment)
		case len(o.lineBuf)+len(segment) <= maxSSEEventBytes:
			o.lineBuf = append(o.lineBuf, segment...)
		default:
			o.overflowTail = o.overflowTail[:0]
			o.keepOverflowTail(o.lineBuf)
			o.keepOverflowTail(segment)
			o.lineBuf = o.lineBuf[:0]
			o.lineOverflow = true
		}

		if completeLine {
			if o.lineOverflow {
				o.observeOversizedTail()
			} else {
				o.observeLine(o.lineBuf, elapsedMs)
			}
			o.lineBuf = o.lineBuf[:0]
			o.lineOverflow = false
		}
	}
}

// overflowTailBytes is how much of an oversized line's end is kept.
const overflowTailBytes = 64 << 10

func (o *sseObservation) keepOverflowTail(segment []byte) {
	o.overflowTail = append(o.overflowTail, segment...)
	if excess := len(o.overflowTail) - overflowTailBytes; excess > 0 {
		o.overflowTail = append(o.overflowTail[:0], o.overflowTail[excess:]...)
	}
}

// observeOversizedTail reads the usage from the end of a line too long to
// parse. A generated image and its usage share one event, and a large image
// pushed the event past the buffer: the usage went with it and the request
// was not billed. The usage object follows the image data, which being base64
// holds no quote to mistake for a key.
func (o *sseObservation) observeOversizedTail() {
	tail := o.overflowTail
	o.overflowTail = o.overflowTail[:0]

	index := bytes.LastIndex(tail, []byte(`"usage":`))
	if index < 0 {
		return
	}
	raw := tail[index+len(`"usage":`):]

	if fields, ok := openaiUsageFromJSON(raw); ok {
		o.openaiUsage.merge(fields)
		o.finalUsageSeen = true
		return
	}
	if fields, ok := anthropicUsageFromJSON(raw); ok {
		o.anthropicUsage.merge(fields)
		o.finalUsageSeen = true
	}
}

// finish observes a trailing partial line and settles a pending terminal event.
func (o *sseObservation) finish(elapsedMs func() int32) {
	if o.lineOverflow {
		o.observeOversizedTail()
	} else if len(o.lineBuf) > 0 {
		o.observeLine(o.lineBuf, elapsedMs)
	}
	o.lineBuf = o.lineBuf[:0]
	o.lineOverflow = false

	if o.terminalEventPending {
		o.terminalEventSeen = true
		o.terminalEventPending = false
	}
}

// ExtractResponseReasoningEffort reads the effort a response reported.
func ExtractResponseReasoningEffort(rawBody []byte, contentType string) *string {
	if strings.Contains(contentType, "event-stream") || bytes.HasPrefix(rawBody, []byte("data:")) {
		var reported *string
		var eventName string
		forEachSSELine(rawBody, func(line []byte) bool {
			eventName = sseFrameName(line, eventName)
			data, ok := sseDataBytes(line)
			if !ok {
				return true
			}
			event := decodeStreamEvent(bytes.TrimSpace(data), eventName)
			if event.hasEffort {
				effort := event.effort
				reported = &effort
				return false
			}
			return true
		})
		return reported
	}

	if effort, ok := responseReasoningEffort(rawBody); ok {
		return &effort
	}
	return nil
}

// responseCapture keeps a bounded prefix of a body while counting its true length.
type responseCapture struct {
	bytes      []byte
	byteLength int
	limit      int
}

func newResponseCapture(limit int) *responseCapture {
	return &responseCapture{limit: limit}
}

func (c *responseCapture) push(chunk []byte) {
	c.byteLength += len(chunk)
	remaining := c.limit - len(c.bytes)
	if remaining <= 0 {
		return
	}
	c.bytes = append(c.bytes, chunk[:min(len(chunk), remaining)]...)
}
