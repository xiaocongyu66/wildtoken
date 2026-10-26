package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/liguangsheng/wildtoken/internal/apperr"
	"github.com/liguangsheng/wildtoken/internal/imagestore"
	"github.com/liguangsheng/wildtoken/internal/logbody"
	"github.com/liguangsheng/wildtoken/internal/metrics"
	"github.com/liguangsheng/wildtoken/internal/models"
)

// attemptTimeout bounds one upstream attempt and reports whether it was the
// thing that ended it.
//
// The bound is on silence, not on total duration. A deadline over the whole
// attempt cut off streaming answers for the offence of being long, which is
// exactly what a reasoning model produces; each chunk that arrives restarts the
// clock, so what remains bounded is "the upstream stopped sending".
//
// Knowing whether the clock ran out is what separates a gateway timeout from a
// client that walked away, since both reach the read as a cancelled context.
type attemptTimeout struct {
	cancel  context.CancelFunc
	timer   *time.Timer
	window  time.Duration
	expired atomic.Bool
	// streaming is set once a stream is being relayed. From then on a client
	// that leaves gets drainGrace of the upstream read for its usage rather
	// than an immediate cancel.
	streaming atomic.Bool
	unfollow  func() bool
}

// drainGrace bounds how long an abandoned stream is still read for its usage.
// The usage follows the last content within milliseconds; what the grace cuts
// off is a long answer nobody is waiting for.
const drainGrace = 2 * time.Second

// newAttemptTimeout starts an attempt whose context follows the client's by
// hand. Derived from it directly, a client leaving cancelled the upstream read
// before the usage arriving after the content could be seen, and a stream
// abandoned there was never billed.
func newAttemptTimeout(clientCtx context.Context, window time.Duration) (context.Context, *attemptTimeout) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(clientCtx))
	timeout := &attemptTimeout{cancel: cancel, window: window}
	timeout.timer = time.AfterFunc(window, func() {
		// Recorded before cancelling, so a reader woken by the cancellation
		// always sees the reason for it.
		timeout.expired.Store(true)
		cancel()
	})
	timeout.unfollow = context.AfterFunc(clientCtx, func() {
		if timeout.streaming.Load() {
			time.AfterFunc(drainGrace, cancel)
			return
		}
		cancel()
	})
	return ctx, timeout
}

// extend restarts the clock: the attempt is waiting on the upstream again.
func (t *attemptTimeout) extend() { t.timer.Reset(t.window) }

// pause stops the clock while the caller is busy downstream. A slow client is
// not the upstream going quiet; counted as such, it got the channel a 504 and
// a health penalty.
func (t *attemptTimeout) pause() { t.timer.Stop() }

// Expired reports whether this timeout ended the attempt.
func (t *attemptTimeout) Expired() bool { return t.expired.Load() }

// stop releases the timer and the attempt's context.
func (t *attemptTimeout) stop() {
	t.unfollow()
	t.timer.Stop()
	t.cancel()
}

// IsChannelFault reports whether a failed status is the channel's own doing,
// to be charged to its health at once.
//
// A 5xx, 408 or 429 is: the channel is down, slow or saturated. Any other 4xx
// may be the request's fault instead — a prompt too long for any model — and
// charging those let a few bad requests take every channel out of routing. The
// caller charges them only once another channel has served the same request.
func IsChannelFault(status int) bool {
	return status < 400 || status >= 500 ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// BuildUpstreamURL builds the full upstream URL for a proxied path.
func BuildUpstreamURL(upstream *models.UpstreamRow, path, queryParams string) string {
	base := strings.TrimRight(upstream.BaseURL, "/")
	suffix := strings.TrimLeft(path, "/")

	// A base that already ends in /v1 is not given a second one.
	target := base + "/v1/" + suffix
	if strings.HasSuffix(base, "/v1") {
		target = base + "/" + suffix
	}
	if queryParams != "" {
		target += "?" + queryParams
	}
	return target
}

// ExtractReasoningEffort reads the requested effort from an OpenAI- or
// Anthropic-compatible request body.
//
// It supports the top-level reasoning_effort (chat completions and the o-series),
// the nested reasoning.effort (Responses API style), and the nested
// output_config.effort (Anthropic Messages API style).
func ExtractReasoningEffort(body []byte) *string {
	var request jsonValue
	if err := json.Unmarshal(body, &request); err != nil {
		return nil
	}

	if effort, ok := formatEffort(request["reasoning_effort"]); ok {
		return &effort
	}
	for _, keys := range [][]string{{"reasoning", "effort"}, {"output_config", "effort"}} {
		if text, ok := valueAt(request, keys...).(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return &trimmed
			}
		}
	}
	return nil
}

// EffortMappingsFromRow decodes a channel's stored reasoning-effort rewrites.
//
// The empty and "{}" cases are answered without parsing, because this runs on
// every proxied request and almost no channel configures a rewrite.
func EffortMappingsFromRow(stored string) map[string]string {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var mappings map[string]string
	if err := json.Unmarshal([]byte(trimmed), &mappings); err != nil {
		return nil
	}
	return mappings
}

// rewriteEffort reads one stored effort value and returns the JSON to put back
// in its place, reporting false when the channel maps it to nothing or to what
// it already says.
//
// The lookup is on the lower-cased value because that is the shape the stored
// keys are normalized to; the replacement goes upstream exactly as written. It
// is always written as a string, which is what every effort field these APIs
// define accepts, even where the caller stated theirs as a number.
func rewriteEffort(raw json.RawMessage, mappings map[string]string) (json.RawMessage, bool) {
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil, false
	}
	formatted, ok := formatEffort(current)
	if !ok {
		return nil, false
	}
	replacement, ok := mappings[strings.ToLower(formatted)]
	if !ok || replacement == formatted {
		return nil, false
	}
	encoded, err := json.Marshal(replacement)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// applyEffortMappings rewrites every place a request states its reasoning
// effort, reporting whether it changed anything.
//
// All three locations are rewritten rather than only the first one found. Which
// of them an upstream reads is its own business, so leaving one still holding
// the downstream value would let the original effort through — and a request
// that states two different efforts is one no upstream promises to resolve the
// way the gateway happened to guess.
func applyEffortMappings(request map[string]json.RawMessage, mappings map[string]string) bool {
	if len(mappings) == 0 {
		return false
	}
	changed := false

	if raw, present := request["reasoning_effort"]; present {
		if replacement, ok := rewriteEffort(raw, mappings); ok {
			request["reasoning_effort"] = replacement
			changed = true
		}
	}

	for _, parent := range []string{"reasoning", "output_config"} {
		raw, present := request[parent]
		if !present {
			continue
		}
		// A non-object here is left alone: rewriting it would mean inventing a
		// shape the caller did not send. Decoding into raw messages keeps the
		// siblings of the effort — thinking, verbosity, summary — byte for byte.
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			continue
		}
		effort, present := nested["effort"]
		if !present {
			continue
		}
		replacement, ok := rewriteEffort(effort, mappings)
		if !ok {
			continue
		}
		nested["effort"] = replacement
		reencoded, err := json.Marshal(nested)
		if err != nil {
			continue
		}
		request[parent] = reencoded
		changed = true
	}

	return changed
}

// PrepareUpstreamBody rewrites a JSON request body for its selected upstream.
//
// Streaming Chat Completions responses omit usage by default on many
// OpenAI-compatible upstreams. It is requested explicitly so the gateway can
// consistently record prompt, completion, and total token counts.
//
// effortMappings replaces the reasoning effort the caller asked for with the one
// the channel's upstream understands, so a downstream naming an effort its
// provider does not have is translated rather than refused.
func PrepareUpstreamBody(body []byte, forwardModel *string, path string,
	effortMappings map[string]string) []byte {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}

	changed := applyEffortMappings(request, effortMappings)

	if forwardModel != nil {
		var currentModel string
		if err := json.Unmarshal(request["model"], &currentModel); err == nil &&
			currentModel != *forwardModel {
			encoded, err := json.Marshal(*forwardModel)
			if err == nil {
				request["model"] = encoded
				changed = true
			}
		}
	}

	if strings.Trim(path, "/") == "chat/completions" && requestsStreaming(request) {
		streamOptions := map[string]json.RawMessage{}
		if raw, present := request["stream_options"]; present {
			// A non-object stream_options is replaced rather than merged. null
			// decodes without error into a nil map, which panicked on the write
			// below and dropped the connection.
			if err := json.Unmarshal(raw, &streamOptions); err != nil || streamOptions == nil {
				streamOptions = map[string]json.RawMessage{}
				changed = true
			}
		}
		if !bytes.Equal(streamOptions["include_usage"], []byte("true")) {
			streamOptions["include_usage"] = json.RawMessage("true")
			changed = true
		}
		if encoded, err := json.Marshal(streamOptions); err == nil {
			request["stream_options"] = encoded
		}
	}

	if !changed {
		return body
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}

func requestsStreaming(request map[string]json.RawMessage) bool {
	var stream bool
	return json.Unmarshal(request["stream"], &stream) == nil && stream
}

// PreparedRequest is everything derived from one attempt's request: the URL,
// headers, upstream body, and their log snapshots.
//
// It is computed once and shared by the caller's abort-log fallback and the
// real upstream call, instead of each redoing the same JSON parsing and
// truncation work.
type PreparedRequest struct {
	URL            string
	ForwardHeaders map[string]string
	UpstreamBody   []byte
	// ReasoningEffort is what the caller asked for; UpstreamReasoningEffort is
	// what the upstream was actually sent, which differ once the channel's
	// effort mapping has rewritten one into the other.
	ReasoningEffort         *string
	UpstreamReasoningEffort *string
	DownstreamSnapshot      *logbody.Snapshot
	UpstreamSnapshot        *logbody.Snapshot
}

// PrepareRequest resolves one attempt against its selected upstream.
func PrepareRequest(downstreamHeaders http.Header, upstream *models.UpstreamRow,
	method, path, queryParams string, forwardModel *string, body []byte,
	logBodyMaxBytes int) (*PreparedRequest, error) {
	url := BuildUpstreamURL(upstream, path, queryParams)
	forwardHeaders, err := BuildForwardHeaders(downstreamHeaders, upstream, path)
	if err != nil {
		return nil, err
	}

	effortMappings := EffortMappingsFromRow(upstream.EffortMappings)
	upstreamBody := PrepareUpstreamBody(body, forwardModel, path, effortMappings)

	// Both efforts are logged: the one the caller asked for, and the one the
	// upstream was actually sent. Without the second, a channel that rewrites
	// "max" into "xhigh" leaves a log saying the request ran at "max", which is
	// the one thing that did not happen.
	//
	// The upstream value is re-read from the prepared body rather than inferred
	// from the mapping table, so it reports what was really sent. That read is
	// skipped when the channel maps nothing, because then nothing rewrote the
	// effort and the two are the same string.
	requestEffort := ExtractReasoningEffort(body)
	upstreamEffort := requestEffort
	if len(effortMappings) > 0 {
		upstreamEffort = ExtractReasoningEffort(upstreamBody)
	}

	return &PreparedRequest{
		URL:                     url,
		ForwardHeaders:          forwardHeaders,
		UpstreamBody:            upstreamBody,
		ReasoningEffort:         requestEffort,
		UpstreamReasoningEffort: upstreamEffort,
		DownstreamSnapshot: SnapshotRequest(method, url, forwardHeaders, body,
			logBodyMaxBytes),
		UpstreamSnapshot: SnapshotRequest(method, url, forwardHeaders, upstreamBody,
			logBodyMaxBytes),
	}, nil
}

// Response is a proxied upstream response. Body must always be closed.
type Response struct {
	Status  int
	Headers map[string]string
	Body    io.ReadCloser
}

// Deps are the shared services a proxied request needs.
type Deps struct {
	HTTPClient *http.Client
	AutoWeight *AutoWeightManager
	Metrics    *metrics.Runtime
	LogWriter  *LogWriter
	// Images moves generated images out of logged bodies into files. Nil or
	// disabled leaves bodies as they are.
	Images         *imagestore.Store
	DefaultTimeout time.Duration
}

// IsImagePath reports whether a proxied path is an image endpoint
// (images/generations, images/edits, …), whose responses carry base64 images.
func IsImagePath(path string) bool {
	return strings.HasPrefix(strings.TrimPrefix(path, "/v1/"), "images/") ||
		strings.HasPrefix(strings.TrimPrefix(path, "/"), "images/")
}

// RequestContext identifies the caller and the model for one proxied request.
type RequestContext struct {
	DownstreamTokenID   int64
	DownstreamTokenName string
	// ClientIP is the caller's address; empty when it could not be resolved.
	ClientIP        string
	ClientType      string
	RequestModel    *string
	ForwardModel    *string
	Method          string
	Path            string
	LogBodyMaxBytes int
}

// ProxyRequest forwards a request upstream, streaming SSE bodies as they arrive.
func ProxyRequest(ctx context.Context, deps Deps, policy AutoWeightPolicy,
	upstream *models.UpstreamRow, requestCtx RequestContext,
	prepared *PreparedRequest) (*Response, error) {
	start := time.Now()
	autoWeightEnabled := upstream.AutoWeightEnabled == 1

	timeout := deps.DefaultTimeout
	if upstream.TimeoutSeconds > 0 {
		timeout = time.Duration(upstream.TimeoutSeconds * float64(time.Second))
	}
	attemptCtx, attempt := newAttemptTimeout(ctx, timeout)

	request, err := buildUpstreamRequest(attemptCtx, requestCtx.Method, prepared)
	if err != nil {
		attempt.stop()

		// The channel's own configuration is what fails here — a base URL the
		// request builder will not accept. Charging it is what eventually takes
		// it out of routing; without that it keeps full weight and is chosen
		// again for every request it is going to fail in the same way.
		deps.AutoWeight.RecordFailure(upstream.ID, autoWeightEnabled, policy)

		// Logged here because the caller disarms its own fallback entry on any
		// error, trusting that the attempt logged itself. This was the one path
		// that did not, so the request left no trace at all.
		message := err.Error()
		statusCode := int32(502)
		entry := baseLogEntry(requestCtx, upstream, prepared)
		entry.StatusCode = &statusCode
		entry.DurationMs = elapsedMs(start)
		entry.Error = &message
		deps.LogWriter.Schedule(entry)

		return nil, apperr.Upstream("the channel's base URL is invalid")
	}

	response, err := deps.HTTPClient.Do(request)
	if err != nil {
		attempt.stop()

		// A client that walks away cancels this request, and the failure that
		// surfaces here looks like any other. It is not the channel's doing, so
		// it is reported as a client abort and left out of the health score.
		clientGone := !attempt.Expired() && ctx.Err() != nil

		statusCode := int32(502)
		switch {
		case clientGone:
			statusCode = 499
		case attempt.Expired():
			// The attempt's own clock ran out: a gateway timeout.
			statusCode = 504
		}
		if !clientGone {
			deps.AutoWeight.RecordFailure(upstream.ID, autoWeightEnabled, policy)
		}

		message := err.Error()
		entry := baseLogEntry(requestCtx, upstream, prepared)
		entry.StatusCode = &statusCode
		entry.DurationMs = elapsedMs(start)
		entry.Error = &message
		deps.LogWriter.Schedule(entry)

		return nil, attemptFailure(statusCode)
	}

	responseHeaders := flattenHeaders(response.Header)
	contentType := responseHeaders["content-type"]
	status := response.StatusCode

	if status >= 200 && status < 300 && IsSSEContentType(contentType) {
		entry := baseLogEntry(requestCtx, upstream, prepared)
		entry.Stream = true
		statusCode := int32(status)
		entry.StatusCode = &statusCode

		// An image stream is kept whole so its images can be saved; anything
		// else keeps only the part the log can hold.
		captureBytes := requestCtx.LogBodyMaxBytes
		if deps.Images.Enabled() && IsImagePath(requestCtx.Path) {
			captureBytes = max(captureBytes, imagestore.MaxCaptureBytes)
		}
		stream := newSSEStream(ctx, response.Body, attempt, start, status, responseHeaders,
			requestCtx.LogBodyMaxBytes, captureBytes, entry, deps, policy, autoWeightEnabled, upstream.ID,
			prepared.UpstreamBody)
		attempt.streaming.Store(true)
		return &Response{Status: status, Headers: responseHeaders, Body: stream}, nil
	}

	bodyBytes, observation, err := readResponseBody(response.Body, start, attempt.extend)
	response.Body.Close()
	attempt.stop()
	if err != nil {
		clientGone := !attempt.Expired() && ctx.Err() != nil

		statusCode := int32(502)
		switch {
		case clientGone:
			statusCode = 499
		case attempt.Expired():
			statusCode = 504
		}
		if !clientGone {
			deps.AutoWeight.RecordFailure(upstream.ID, autoWeightEnabled, policy)
		}

		message := err.Error()
		entry := baseLogEntry(requestCtx, upstream, prepared)
		entry.StatusCode = &statusCode
		entry.DurationMs = elapsedMs(start)
		entry.Error = &message
		deps.LogWriter.Schedule(entry)
		return nil, attemptFailure(statusCode)
	}

	// A 2xx body that is a stream can still end on an error event.
	var streamError *string
	if status >= 200 && status < 300 {
		streamError = observation.streamError
	}

	switch {
	case streamError != nil:
		if observation.streamErrorFault {
			deps.AutoWeight.RecordFailure(upstream.ID, autoWeightEnabled, policy)
		}
	case status >= 200 && status < 300:
		deps.AutoWeight.RecordSuccess(upstream.ID, autoWeightEnabled, policy)
	case IsChannelFault(status):
		deps.AutoWeight.RecordFailure(upstream.ID, autoWeightEnabled, policy)
	}

	responseSnapshot := SnapshotResponse(status, responseHeaders, deps.Images.Rewrite(bodyBytes),
		requestCtx.LogBodyMaxBytes)
	isStream := bytes.HasPrefix(bodyBytes, []byte("data:")) ||
		bytes.HasPrefix(bytes.TrimSpace(bodyBytes), []byte("event:")) ||
		strings.Contains(contentType, "event-stream")
	// A stream sent as text/plain is read as one too; parsed as JSON, its
	// usage came back empty and the request went unbilled.
	usageContentType := contentType
	if isStream {
		usageContentType = "text/event-stream"
	}
	usage := ExtractUsage(bodyBytes, usageContentType)

	// A true streamed time-to-first-token is preferred; buffered detection is
	// only a fallback, and only for stream bodies.
	var firstTokenMs *int32
	if isStream {
		firstTokenMs = observation.firstTokenMs
		if firstTokenMs == nil && HasVisibleToken(bodyBytes) {
			firstTokenMs = elapsedMs(start)
		}
	}

	entry := baseLogEntry(requestCtx, upstream, prepared)
	statusCode := int32(status)
	entry.StatusCode = &statusCode
	if streamError != nil {
		failed := int32(http.StatusBadGateway)
		entry.StatusCode = &failed
		entry.Error = streamError
	}
	entry.Stream = isStream
	entry.ResponseReasoningEffort = ExtractResponseReasoningEffort(bodyBytes, usageContentType)
	entry.PromptTokens = usage.PromptTokens
	entry.CompletionTokens = usage.CompletionTokens
	entry.TotalTokens = usage.TotalTokens
	entry.PromptCachedTokens = usage.PromptCachedTokens
	entry.CacheCreationTokens = usage.CacheCreationTokens
	entry.CompletionReasoningTokens = usage.CompletionReasoningTokens
	entry.FirstTokenMs = firstTokenMs
	entry.DurationMs = elapsedMs(start)
	entry.UpstreamResponse = responseSnapshot
	entry.DownstreamResponse = responseSnapshot

	// Scheduled when the body is closed rather than here. Here is before the
	// response has reached the client at all, so a client that leaves during
	// delivery would be recorded as having received what the upstream sent.
	return &Response{
		Status:  status,
		Headers: responseHeaders,
		Body:    newBufferedStream(ctx, bodyBytes, entry, deps, statusCode),
	}, nil
}

// attemptFailure is what the caller is told about an attempt that got no
// answer. The transport's error names the upstream URL, query and all, which
// is the operator's to read in the log; returned as is, it went to whoever
// held a token. A timeout answers 504, as the log records it.
func attemptFailure(statusCode int32) error {
	if statusCode == http.StatusGatewayTimeout {
		return apperr.GatewayTimeout("the upstream did not respond in time")
	}
	return apperr.Upstream("the upstream request failed")
}

func buildUpstreamRequest(ctx context.Context, method string, prepared *PreparedRequest) (*http.Request, error) {
	var body io.Reader
	if len(prepared.UpstreamBody) > 0 {
		body = bytes.NewReader(prepared.UpstreamBody)
	}

	request, err := http.NewRequestWithContext(ctx, method, prepared.URL, body)
	if err != nil {
		return nil, apperr.Upstream(err.Error())
	}
	for name, value := range prepared.ForwardHeaders {
		if containsFold(HopByHopHeaders, name) {
			continue
		}
		request.Header.Set(name, value)
	}
	return request, nil
}

// baseLogEntry fills the fields every attempt reports, regardless of outcome.
func baseLogEntry(requestCtx RequestContext, upstream *models.UpstreamRow,
	prepared *PreparedRequest) LogEntry {
	upstreamID := upstream.ID
	upstreamName := upstream.Name
	tokenID := requestCtx.DownstreamTokenID
	tokenName := requestCtx.DownstreamTokenName
	clientType := requestCtx.ClientType

	// nil rather than a pointer to "": the column means "not known", and an
	// empty string would render as a blank cell instead of a dash.
	var clientIP *string
	if requestCtx.ClientIP != "" {
		address := requestCtx.ClientIP
		clientIP = &address
	}

	return LogEntry{
		Method:                  requestCtx.Method,
		Path:                    requestCtx.Path,
		DownstreamTokenID:       &tokenID,
		DownstreamTokenName:     &tokenName,
		ClientIP:                clientIP,
		ClientType:              &clientType,
		UpstreamID:              &upstreamID,
		UpstreamName:            &upstreamName,
		Model:                   requestCtx.ForwardModel,
		RequestModel:            requestCtx.RequestModel,
		UpstreamModel:           requestCtx.ForwardModel,
		ReasoningEffort:         prepared.ReasoningEffort,
		UpstreamReasoningEffort: prepared.UpstreamReasoningEffort,
		DownstreamRequest:       prepared.DownstreamSnapshot,
		UpstreamRequest:         prepared.UpstreamSnapshot,
	}
}

func elapsedMs(start time.Time) *int32 {
	measured := int32(time.Since(start).Milliseconds())
	return &measured
}

func flattenHeaders(headers http.Header) map[string]string {
	flattened := make(map[string]string, len(headers))
	for name, values := range headers {
		if len(values) > 0 {
			flattened[strings.ToLower(name)] = values[0]
		}
	}
	return flattened
}

// MaxUpstreamResponseBytes caps a buffered upstream response.
//
// The downstream request body is already bounded, but the response was not: a
// misbehaving or compromised channel could return a body large enough to exhaust
// the gateway's memory, and a handful of concurrent ones could do it outright.
// The limit is far above any real completion, so it only ever catches a channel
// that is not answering in good faith.
const MaxUpstreamResponseBytes = 128 << 20

// ErrUpstreamResponseTooLarge reports a buffered response that ran past the cap.
var ErrUpstreamResponseTooLarge = errors.New("upstream response exceeded the maximum buffered size")

// readResponseBody reads a full upstream body while observing it as an SSE
// stream, for the true time-to-first-token and any error event it carries.
//
// progress is called for each chunk, so the attempt's clock measures silence
// from the upstream rather than the total time a long body takes to arrive.
func readResponseBody(body io.Reader, start time.Time, progress func()) ([]byte, *sseObservation, error) {
	var collected bytes.Buffer
	observation := &sseObservation{}
	measure := func() int32 { return int32(time.Since(start).Milliseconds()) }

	buffer := make([]byte, 32*1024)
	for {
		read, err := body.Read(buffer)
		if read > 0 {
			if collected.Len()+read > MaxUpstreamResponseBytes {
				return nil, nil, ErrUpstreamResponseTooLarge
			}
			progress()
			chunk := buffer[:read]
			collected.Write(chunk)
			observation.observeChunk(chunk, measure)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
	}

	// The final partial line is observed too, keeping parity with buffered
	// detection.
	observation.finish(measure)
	return collected.Bytes(), observation, nil
}
