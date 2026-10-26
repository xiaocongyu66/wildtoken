package proxy

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// sseStream forwards an upstream SSE body downstream while observing it, and
// writes the request log once the stream ends for any reason.
//
// The Rust implementation relied on Drop to log a client disconnect. Go has no
// destructor, so the same guarantee comes from the HTTP server always closing a
// response body: Close is what finalizes an abandoned stream.
type sseStream struct {
	upstream io.ReadCloser
	// requestCtx is the downstream request's context, which is what tells a
	// client walking away apart from an upstream failing.
	requestCtx  context.Context
	attempt     *attemptTimeout
	capture     *responseCapture
	observation *sseObservation
	start       time.Time

	upstreamStatus    int
	responseHeaders   map[string]string
	logBodyMaxBytes   int
	deps              Deps
	policy            AutoWeightPolicy
	autoWeightEnabled bool
	upstreamID        int64

	// mu guards entry so that whichever path reaches the end of the stream first
	// is the one that logs it: the terminal event, the EOF after it, a failed
	// read, or Close.
	//
	// It guards entry only. Everything else here is written from the read loop
	// and read again in Close, which is safe because both run in the handler's
	// own goroutine — a caller that closed the body from another one would be
	// racing the fields this lock does not cover.
	mu    sync.Mutex
	entry *LogEntry

	// requestBody is what was sent upstream, for estimating the prompt of a
	// stream abandoned before its usage arrived.
	requestBody []byte
}

func newSSEStream(requestCtx context.Context, upstream io.ReadCloser, attempt *attemptTimeout,
	start time.Time, status int,
	responseHeaders map[string]string, logBodyMaxBytes, captureBytes int, entry LogEntry,
	deps Deps, policy AutoWeightPolicy, autoWeightEnabled bool, upstreamID int64,
	requestBody []byte) *sseStream {
	deps.Metrics.StartSSEStream()
	return &sseStream{
		upstream:          upstream,
		requestCtx:        requestCtx,
		attempt:           attempt,
		capture:           newResponseCapture(captureBytes),
		observation:       &sseObservation{},
		start:             start,
		upstreamStatus:    status,
		responseHeaders:   responseHeaders,
		logBodyMaxBytes:   logBodyMaxBytes,
		deps:              deps,
		policy:            policy,
		autoWeightEnabled: autoWeightEnabled,
		upstreamID:        upstreamID,
		entry:             &entry,
		requestBody:       requestBody,
	}
}

func (s *sseStream) Read(buffer []byte) (int, error) {
	// The clock runs only while waiting on the upstream, so a long answer is
	// never cut off for being long, nor a slow reader for being slow.
	s.attempt.extend()
	read, err := s.upstream.Read(buffer)
	s.attempt.pause()
	if read > 0 {
		chunk := buffer[:read]
		s.capture.push(chunk)
		s.observation.observeChunk(chunk, s.measure)
		if s.observation.terminalEventSeen {
			s.finishComplete()
		}
	}

	switch {
	case err == io.EOF:
		s.finishComplete()
	case err != nil:
		s.finishInterrupted(err)
	}
	return read, err
}

// Close finalizes a stream the client abandoned before it completed.
func (s *sseStream) Close() error {
	s.mu.Lock()
	pending := s.entry != nil
	s.mu.Unlock()

	if pending {
		// Delivered in full except the events after the last read: the usage
		// among them is read before the log is written. A terminal event that
		// only lacks its closing blank line has arrived; finish settles it.
		drained := false
		if !s.observation.terminalEventSeen && !s.observation.terminalEventPending {
			s.drain()
			drained = true
		}
		s.observation.finish(s.measure)
		if s.observation.terminalEventSeen && !drained {
			s.finishComplete()
		} else if s.finishLog(499,
			ptrTo("client disconnected before the SSE response completed")) {
			s.deps.Metrics.RecordSSEClientDisconnect()
		}
	}

	s.deps.Metrics.FinishSSEStream()
	err := s.upstream.Close()
	s.attempt.stop()
	return err
}

// drain keeps reading an abandoned stream, for at most drainGrace, until its
// terminal event. The usage comes last: a client that stopped reading at the
// final content, whether it meant to or not, left before it, and the request
// was logged without usage and never billed.
func (s *sseStream) drain() {
	deadline := time.AfterFunc(drainGrace, s.attempt.cancel)
	defer deadline.Stop()

	buffer := make([]byte, 32<<10)
	for !s.observation.terminalEventSeen {
		s.attempt.extend()
		read, err := s.upstream.Read(buffer)
		s.attempt.pause()
		if read > 0 {
			chunk := buffer[:read]
			s.capture.push(chunk)
			s.observation.observeChunk(chunk, s.measure)
		}
		if err != nil {
			return
		}
	}
}

func (s *sseStream) measure() int32 {
	return int32(time.Since(s.start).Milliseconds())
}

// recordResponseHealth credits a stream that ran to its end.
//
// A stream is only built for a 2xx response, so reaching the end of one is
// always a success. The status is not re-examined here: doing so implied this
// type could carry a failing status, which it cannot, and left a branch that
// looked covered while being unreachable.
func (s *sseStream) recordResponseHealth() {
	s.deps.AutoWeight.RecordSuccess(s.upstreamID, s.autoWeightEnabled, s.policy)
}

// finishComplete logs a stream that reached its end.
//
// The health score is recorded behind the same guard as the log, because a
// stream reaches here more than once: the terminal event finishes it, and so
// does the EOF that follows. Recording outside the guard scored one stream
// twice and let a penalised channel recover at double the configured rate.
//
// A stream that ended on an error event failed, though it began as a 200.
// Logged as the 200, the failure was invisible to the log filters and the
// dashboard, and the channel was credited for it.
func (s *sseStream) finishComplete() {
	s.observation.finish(s.measure)
	if failure := s.observation.streamError; failure != nil {
		if s.finishLog(502, failure) {
			if s.observation.streamErrorFault {
				s.deps.AutoWeight.RecordFailure(s.upstreamID, s.autoWeightEnabled, s.policy)
			}
			s.deps.Metrics.RecordSSEUpstreamError()
		}
		return
	}

	if s.finishLog(int32(s.upstreamStatus), nil) {
		s.recordResponseHealth()
		s.deps.Metrics.RecordSSEComplete()
	}
}

// finishInterrupted logs a stream that stopped before its terminal event,
// attributing it to whichever side actually ended it.
//
// A client that walks away cancels the request context, and the read fails with
// a cancellation indistinguishable from an upstream one. Charging every such
// read to the channel is what let ordinary use — a user pressing escape on a
// long answer — drive a healthy channel's weight to zero and take it out of
// rotation, while leaving the disconnect metric reading zero.
func (s *sseStream) finishInterrupted(err error) {
	// Client aborts are the one outcome the channel is not answerable for. The
	// health score, like the log, is recorded behind finishLog's guard: a stream
	// that already finished on its terminal event can still fail the read that
	// follows, and scoring outside the guard charged that channel for a stream
	// the log had already recorded as a success.
	message := err.Error()
	switch {
	case s.attempt.Expired():
		// The upstream went quiet for longer than the channel allows.
		if s.finishLog(504, &message) {
			s.deps.AutoWeight.RecordFailure(s.upstreamID, s.autoWeightEnabled, s.policy)
			s.deps.Metrics.RecordSSEUpstreamError()
		}
	case s.requestCtx.Err() != nil:
		if s.finishLog(499,
			ptrTo("client disconnected before the SSE response completed")) {
			s.deps.Metrics.RecordSSEClientDisconnect()
		}
	default:
		if s.finishLog(502, &message) {
			s.deps.AutoWeight.RecordFailure(s.upstreamID, s.autoWeightEnabled, s.policy)
			s.deps.Metrics.RecordSSEUpstreamError()
		}
	}
}

// finishLog writes the request log once. It reports false when another path
// already logged this stream.
// snapshotResponse builds the logged response. An image stream is captured
// whole so its images can be saved as files; the log body cap then applies to
// what is left. A capture that ran past its bound is logged as before, since a
// cut-off event cannot be decoded.
func (s *sseStream) snapshotResponse() json.RawMessage {
	body, byteLength := s.capture.bytes, s.capture.byteLength
	if byteLength == len(body) {
		body = s.deps.Images.Rewrite(body)
		byteLength = len(body)
	}
	return SnapshotResponseWithBodyLength(s.upstreamStatus, s.responseHeaders,
		body, byteLength, s.logBodyMaxBytes)
}

func (s *sseStream) finishLog(statusCode int32, streamError *string) bool {
	s.mu.Lock()
	entry := s.entry
	s.entry = nil
	s.mu.Unlock()

	if entry == nil {
		return false
	}

	s.observation.finish(s.measure)
	usage := s.observation.tokenUsage()
	// A stream the client abandoned before the upstream's final usage is
	// billed on an estimate. The upstream still charges the operator for the
	// prompt and what it generated; logged as nothing, stopping just short of
	// the usage was a way to spend a quota without it counting.
	if statusCode == 499 && !s.observation.finalUsageSeen {
		usage = s.estimateUsage(usage)
		message := "usage estimated"
		if streamError != nil {
			message = *streamError + "; " + message
		}
		streamError = &message
	}
	responseSnapshot := s.snapshotResponse()

	entry.StatusCode = &statusCode
	entry.ResponseReasoningEffort = s.observation.responseReasoningEffort
	entry.PromptTokens = usage.PromptTokens
	entry.CompletionTokens = usage.CompletionTokens
	entry.TotalTokens = usage.TotalTokens
	entry.PromptCachedTokens = usage.PromptCachedTokens
	entry.CacheCreationTokens = usage.CacheCreationTokens
	entry.CompletionReasoningTokens = usage.CompletionReasoningTokens
	entry.FirstTokenMs = s.observation.firstTokenMs
	entry.DurationMs = elapsedMs(s.start)
	entry.Error = streamError
	entry.UpstreamResponse = responseSnapshot
	entry.DownstreamResponse = responseSnapshot

	s.deps.LogWriter.Schedule(*entry)
	return true
}

func ptrTo[T any](value T) *T { return &value }

// estimateUsage fills what an abandoned stream did not report: the prompt from
// the request when message_start gave none, the completion from the text that
// streamed. Anthropic's message_start reports output_tokens 1 as a placeholder,
// so a reported completion only stands when it is larger.
func (s *sseStream) estimateUsage(reported TokenUsage) TokenUsage {
	prompt := reported.PromptTokens
	if prompt == nil || *prompt == 0 {
		// A zero prompt is a placeholder, not a report: every request carries
		// at least some input. Leaving it at zero would bill the stream unpaid.
		prompt = ptrTo(clampTokens(estimateRequestTokens(s.requestBody)))
	}
	completion := ptrTo(clampTokens(s.observation.outputEstimate))
	if reported.CompletionTokens != nil && *reported.CompletionTokens > *completion {
		completion = reported.CompletionTokens
	}

	estimated := reported
	estimated.PromptTokens = prompt
	estimated.CompletionTokens = completion
	estimated.TotalTokens = sumTokenParts(prompt, completion)
	return estimated
}
