package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gosuda/gyemoim/internal/gateway"
	"github.com/gosuda/gyemoim/internal/history"
	"github.com/gosuda/gyemoim/internal/provider"
	"github.com/gosuda/gyemoim/internal/siwc"
)

// commitmentWriter tracks attempted commitment, not just successful writes. Once
// headers or bytes might have reached the client, another target is forbidden.
type commitmentWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *commitmentWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *commitmentWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *commitmentWriter) Write(body []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(body)
}

type executionResult struct {
	history.AttemptResult
	failure         *gateway.Failure
	status          int // safe downstream status, distinct from the upstream HTTP status
	errorType, code string
	capability      *provider.CapabilityError
	retryAfter      string
	recordingFailed bool
	delivered       bool
}

func failedExecution(kind gateway.FailureKind, status int, message, code string) executionResult {
	return executionResult{AttemptResult: history.AttemptResult{Outcome: "failed", Reason: code, SafeError: message},
		failure: &gateway.Failure{Kind: kind}, status: status, errorType: "server_error", code: code}
}

func recordingExecution() executionResult {
	result := failedExecution(gateway.FailureRecording, http.StatusServiceUnavailable, "request recording became unavailable before provider transmission", "recording_unavailable")
	result.recordingFailed = true
	return result
}

// executeInference owns one recorder handle, one request ID and one admission
// slot. Every candidate has one lifecycle and at most one transmission; selector
// decisions, rather than an executor retry loop, define the complete order.
func (api *harnessAPI) executeInference(rawWriter http.ResponseWriter, r *http.Request, route gateway.ResolvedRoute, incoming, source json.RawMessage, format inferenceFormat, includeUsage bool, drops []string, singlePreparation siwc.AccessTokenPreparation) {
	releaseAdmission := func() {
		if singlePreparation != nil {
			singlePreparation.Close()
			singlePreparation = nil
		}
	}
	defer releaseAdmission()
	w := &commitmentWriter{ResponseWriter: rawWriter}
	handle, err := api.recorder.Begin(history.Start{
		ServiceAccount: history.ServiceAccountSnapshot{ID: route.Identity.Account.ID, Name: route.Identity.Account.Name},
		Model:          history.ModelSnapshot{ID: route.Model.ID, Name: route.Model.Name, Version: route.Model.Version, Strategy: route.Model.Strategy}, IncomingRequest: incoming,
	})
	if err != nil {
		releaseAdmission()
		writeHarnessError(w, http.StatusServiceUnavailable, "request recording is unavailable", "server_error", "recording_unavailable")
		return
	}
	w.Header().Set("X-Request-ID", handle.ID())
	result := failedExecution(gateway.FailureRequest, http.StatusInternalServerError, "the configured model route is unavailable", "route_unavailable")
	defer func() {
		_ = handle.EndWithDetails(result.Outcome, result.HTTPStatus, result.SafeError, result.Usage, result.Timings,
			history.EndDetails{UpstreamRequestID: result.UpstreamRequestID, DroppedFields: drops})
	}()
	selection, ok := route.Selection.(gateway.DecisionSelection)
	if !ok {
		api.deliverExecution(w, r, handle, &result, format)
		return
	}
	var previous *gateway.AttemptOutcome
	var last *executionResult
	transmissions := 0
	for {
		decision, err := selection.NextDecision(r.Context(), previous)
		previous = nil
		if err != nil {
			if last != nil {
				result = *last
			} else {
				result = failedExecution(gateway.FailureDisconnected, http.StatusServiceUnavailable, "no configured provider is currently available", "route_unavailable")
				var unavailable *gateway.SelectionUnavailable
				if errors.As(err, &unavailable) && errors.Is(err, gateway.ErrTargetsCooling) {
					result.retryAfter = strconv.FormatInt(unavailable.RetryAfterSeconds(time.Now()), 10)
				}
			}
			api.deliverExecution(w, r, handle, &result, format)
			return
		}
		p := route.Providers[decision.Target.ProviderID]
		candidate := history.Candidate{Number: decision.Position + 1, Provider: history.ProviderSnapshot{ID: p.ID, Name: p.Name, UpstreamModel: decision.Target.UpstreamModel}}
		if decision.Kind == "skip" {
			if err := handle.SkipCandidate(candidate, decision.State.Status); err != nil {
				result = recordingExecution()
				api.deliverExecution(w, r, handle, &result, format)
				return
			}
			continue
		}
		if err := handle.StartAttempt(candidate); err != nil {
			result = recordingExecution()
			api.deliverExecution(w, r, handle, &result, format)
			return
		}
		observation := api.gateway.Runtime().BeginAttempt(decision.Target)
		preparation := singlePreparation
		singlePreparation = nil
		result = api.executeCandidate(w, r, handle, candidate, source, format, route.Model.Name, includeUsage, route.Model.Strategy == "fallback", preparation, &transmissions, &drops)
		if r.Context().Err() != nil {
			result.Outcome, result.Reason, result.SafeError = "cancelled", "canceled", "downstream client cancelled the request"
			result.failure = &gateway.Failure{Kind: gateway.FailureCanceled, ClientCanceled: true}
		}
		if result.failure != nil {
			result.failure.ResponseCommitted = w.committed
			result.failure.RecordingFailed = result.recordingFailed
			api.gateway.Runtime().ObserveFailure(observation, *result.failure, time.Now())
		} else {
			api.gateway.Runtime().ObserveSuccess(observation)
		}
		// executeCandidate has closed its upstream and joined all synchronous
		// event/body callbacks. The next decision requires this durable closure.
		if err := handle.EndAttempt(candidate.Number, result.AttemptResult); err != nil {
			result.recordingFailed = true
			if !w.committed {
				result = recordingExecution()
			}
			api.deliverExecution(w, r, handle, &result, format)
			return
		}
		if result.failure == nil || w.committed || result.recordingFailed || r.Context().Err() != nil {
			api.deliverExecution(w, r, handle, &result, format)
			return
		}
		lastResult := result
		last = &lastResult
		previous = &gateway.AttemptOutcome{Outcome: result.Outcome, HTTPStatus: result.HTTPStatus, Failure: result.failure}
	}
}

func mergeExecutionDrops(drops *[]string, added []string) {
	seen := make(map[string]bool, len(*drops)+len(added))
	merged := make([]string, 0, len(*drops)+len(added))
	for _, list := range [][]string{*drops, added} {
		for _, field := range list {
			if !seen[field] {
				seen[field] = true
				merged = append(merged, field)
			}
		}
	}
	*drops = merged
}

func (api *harnessAPI) executeCandidate(w *commitmentWriter, r *http.Request, handle *history.Request, candidate history.Candidate, source json.RawMessage, format inferenceFormat, alias string, includeUsage, allowFallback bool, preparation siwc.AccessTokenPreparation, transmissions *int, drops *[]string) executionResult {
	// Preparation leases recheck disconnection after the route snapshot. Single
	// may already own its admission lease; fallback acquires one only after start.
	var err error
	if preparation == nil {
		preparation, err = api.tokens.BeginAccessTokenPreparation(r.Context(), candidate.Provider.ID)
	}
	if err != nil {
		message, code := safeProviderAuthError(err)
		kind := gateway.FailureRequest
		if errors.Is(err, siwc.ErrProviderNotReady) {
			kind = gateway.FailureDisconnected
		} else if knownAuthFailure(err) {
			kind = gateway.FailureAuth
		}
		result := failedExecution(kind, http.StatusBadGateway, message, code)
		result.Timings.AuthenticationPreparationNS = int64Pointer(handle.ElapsedNS())
		return result
	}
	leaseOpen := true
	release := func() {
		if leaseOpen {
			preparation.Close()
			leaseOpen = false
		}
	}
	defer release()
	prepared, err := api.adapter.Prepare(candidate.Provider.UpstreamModel, source)
	if err != nil {
		var capability *provider.CapabilityError
		if errors.As(err, &capability) {
			result := failedExecution(gateway.FailureCapability, http.StatusBadRequest, capability.Message, "unsupported_parameter")
			result.code, result.errorType = capability.Code, "invalid_request_error"
			// Preserve the capability param in the final client envelope below.
			result.capability = capability
			return result
		}
		return failedExecution(gateway.FailureRequest, http.StatusBadRequest, "request could not be prepared for the provider", "invalid_request")
	}
	mergeExecutionDrops(drops, prepared.DroppedFields)
	managedToken, err := preparation.AccessToken(r.Context())
	authNS := handle.ElapsedNS()
	release() // Never retain the credential lock over transmit or upstream I/O.
	if err != nil {
		message, code := safeProviderAuthError(err)
		kind := gateway.FailureRequest
		if errors.Is(err, siwc.ErrProviderNotReady) {
			kind = gateway.FailureDisconnected
		} else if knownAuthFailure(err) {
			kind = gateway.FailureAuth
		}
		result := failedExecution(kind, http.StatusBadGateway, message, code)
		result.Timings.AuthenticationPreparationNS = &authNS
		return result
	}
	number := *transmissions + 1
	if err := handle.Transmit(history.Attempt{Number: number, Candidate: candidate.Number, Provider: candidate.Provider, EffectiveRequest: prepared.EffectiveJSON}); err != nil {
		return recordingExecution()
	}
	*transmissions = number
	if r.Context().Err() != nil {
		return failedExecution(gateway.FailureCanceled, http.StatusBadGateway, "downstream client cancelled the request", "canceled")
	}
	trace := provider.NewTraceWithElapsed(handle.ElapsedNS)
	upstream, sendErr := api.adapter.Send(r.Context(), managedToken, handle.ID(), prepared, trace)
	result := failedExecution(gateway.FailureRequest, http.StatusBadGateway, "the provider returned no response", "upstream_error")
	result.Timings = providerTimings(authNS, trace.Snapshot(), nil)
	if upstream != nil {
		defer upstream.Close()
		result.HTTPStatus, result.UpstreamRequestID = upstream.StatusCode, upstream.UpstreamRequestID
		if upstream.StatusCode == http.StatusTooManyRequests {
			result.retryAfter = upstream.RetryAfter
		}
		if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices || upstream.UnexpectedContentType {
			// Preserve exact bounded upstream bytes in history; never return the raw
			// body to a caller because it may include provider details or secrets.
			result.recordingFailed = handle.HTTPResponseForAttempt(number, upstream.StatusCode, upstream.ContentType, upstream.UpstreamRequestID, upstream.ErrorBody, upstream.ErrorBodyTruncated, upstream.ErrorBodyReadFailed) != nil
		}
	}
	if sendErr != nil {
		result.status, result.SafeError, result.errorType, result.code = safeUpstreamSendError(sendErr)
		result.Reason = result.code
		result.failure = boundaryFailure(sendErr)
		return result
	}
	if upstream == nil {
		return result
	}
	if upstream.StatusCode < http.StatusOK || upstream.StatusCode >= http.StatusMultipleChoices || upstream.UnexpectedContentType {
		result.status, result.SafeError, result.errorType, result.code = safeUpstreamHTTPError(upstream.StatusCode, upstream.UnexpectedContentType)
		result.Reason = result.code
		result.failure = &gateway.Failure{Kind: gateway.FailureHTTP, HTTPStatus: upstream.StatusCode, RetryAfter: upstream.RetryAfter}
		if upstream.UnexpectedContentType {
			result.failure.Kind = gateway.FailureProtocol
		}
		return result
	}
	return api.consumeCandidate(w, r, handle, upstream, number, trace, authNS, prepared.ClientStream, format, alias, includeUsage, allowFallback, result)
}

func knownAuthFailure(err error) bool {
	return errors.Is(err, siwc.ErrProviderReauthentication) || errors.Is(err, siwc.ErrProviderAuthUnavailable) || errors.Is(err, siwc.ErrProviderOAuthClient) || errors.Is(err, siwc.ErrProviderNotReady) || errors.Is(err, provider.ErrMissingManagedToken)
}

// Only adapter-confirmed origins authorize fallback. Unknown Send errors and
// invalid local request metadata are NOT assumed to be network failures.
func boundaryFailure(err error) *gateway.Failure {
	f := &gateway.Failure{Kind: gateway.FailureRequest}
	switch {
	case errors.Is(err, context.Canceled):
		f.Kind, f.ClientCanceled = gateway.FailureCanceled, true
	case errors.Is(err, provider.ErrMissingManagedToken):
		f.Kind = gateway.FailureAuth
	case errors.Is(err, provider.ErrUpstreamUnavailable), errors.Is(err, provider.ErrStreamRead), errors.Is(err, provider.ErrUpstreamRead), errors.Is(err, context.DeadlineExceeded):
		f.Kind = gateway.FailureTransport
	case errors.Is(err, provider.ErrNoEventStream), errors.Is(err, provider.ErrInvalidUTF8), errors.Is(err, provider.ErrEventTooLarge), errors.Is(err, io.EOF):
		f.Kind = gateway.FailureProtocol
	}
	return f
}

// initialEventFailure is an allowlist of safe provider facts. An unknown model
// error is terminal, not a malformed-stream or transport failure.
func initialEventFailure(event provider.SSEEvent) *gateway.Failure {
	f := &gateway.Failure{Kind: gateway.FailureInitialStream}
	var envelope struct {
		StatusCode int    `json:"status_code"`
		Code       string `json:"code"`
		Error      struct {
			Code       string `json:"code"`
			StatusCode int    `json:"status_code"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code       string `json:"code"`
				StatusCode int    `json:"status_code"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(event.DataJSON, &envelope) != nil {
		return f
	}
	code, status := envelope.Response.Error.Code, envelope.Response.Error.StatusCode
	if code == "" {
		code = envelope.Error.Code
	}
	if code == "" {
		code = envelope.Code
	}
	if status == 0 {
		status = envelope.Error.StatusCode
	}
	if status == 0 {
		status = envelope.StatusCode
	}
	if status >= http.StatusBadRequest && status <= 599 {
		f.HTTPStatus = status
		return f
	}
	switch code {
	case "rate_limit_exceeded", "rate_limit_error":
		f.HTTPStatus = http.StatusTooManyRequests
	case "server_error", "internal_server_error":
		f.HTTPStatus = http.StatusInternalServerError
	case "service_unavailable", "overloaded_error":
		f.HTTPStatus = http.StatusServiceUnavailable
	case "invalid_api_key", "authentication_error":
		f.HTTPStatus = http.StatusUnauthorized
	}
	return f
}

func (api *harnessAPI) consumeCandidate(w *commitmentWriter, r *http.Request, handle *history.Request, upstream *provider.UpstreamResponse, number int, trace *provider.Trace, authNS int64, streaming bool, format inferenceFormat, alias string, includeUsage, allowFallback bool, result executionResult) executionResult {
	controller := http.NewResponseController(w)
	var delivery *int64
	var chatOutput *provider.ChatOutputAccumulator
	var synthesizer *provider.ChatStreamSynthesizer
	if format == formatChat {
		if streaming {
			synthesizer = provider.NewChatStreamSynthesizer(handle.ID(), alias, time.Now().Unix(), includeUsage)
		} else {
			// The chat delivery accumulates the streamed output because the live SIWC
			// backend sends a terminal response object with an empty output array.
			chatOutput = &provider.ChatOutputAccumulator{}
		}
	}
	finish := func() executionResult {
		result.Timings = providerTimings(authNS, trace.Snapshot(), delivery)
		return result
	}
	writeFrame := func(frame []byte) bool {
		if !w.committed {
			setFinalUpstreamHeaders(w, result)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			// WriteHeader is intentionally left to Write after its deadline is set.
		}
		if err := writeDownstreamFrame(controller, w, frame); err != nil {
			result.Outcome, result.Reason, result.SafeError = "cancelled", "downstream_disconnected", "downstream response could not be delivered"
			result.failure = &gateway.Failure{Kind: gateway.FailureCanceled, ClientCanceled: true}
			return false
		}
		delivery = int64Pointer(handle.ElapsedNS())
		return true
	}
	for {
		event, err := upstream.NextEvent()
		if err != nil {
			result.failure = boundaryFailure(err)
			result.status, result.SafeError, result.errorType, result.code = safeUpstreamStreamError(err)
			if streaming {
				result.SafeError = safeStreamReadError(err)
			}
			if errors.Is(err, io.EOF) {
				result.Outcome, result.code = "incomplete", "upstream_protocol_error"
				result.SafeError = "the provider stream ended before a terminal response"
				if streaming {
					result.SafeError = "provider stream ended without a terminal response event"
				}
			}
			result.Reason = result.code
			return finish()
		}
		// Record each raw frame before interpreting it. The loop keeps no event
		// history in memory, and chat history stays upstream-native.
		if handle.EventForAttempt(number, event.Raw, event.Name) != nil {
			// Already-transmitted streams may continue, but recording failure
			// prevents any later candidate or transmission.
			result.recordingFailed = true
		}
		if terminalProtocolMismatch(event) || event.MalformedData {
			result.failure = &gateway.Failure{Kind: gateway.FailureProtocol}
			result.SafeError, result.code, result.Reason = "the provider stream contained an invalid response event", "upstream_protocol_error", "upstream_protocol_error"
			return finish()
		}
		if event.Terminal && event.Usage != nil {
			result.Usage = usageToHistory(event.Usage)
		}
		if !event.Complete {
			// Keep a final unterminated frame in history, but do not append it to
			// the downstream SSE stream where it could corrupt the next event.
			continue
		}
		if chatOutput != nil {
			chatOutput.Observe(event)
		}
		if event.Terminal && event.Outcome == "failed" {
			result.failure = initialEventFailure(event)
			if result.failure.HTTPStatus == http.StatusTooManyRequests {
				result.failure.RetryAfter, result.retryAfter = upstream.RetryAfter, upstream.RetryAfter
			}
			result.SafeError, result.code, result.Reason = terminalSafeError(event), "provider_response_failed", "provider_response_failed"
			// Only an explicit fallback route may withhold a retryable initial
			// failure. Single and terminal unknown failures keep SSE delivery.
			if streaming && (!allowFallback || w.committed || result.recordingFailed || !gateway.ClassifyFailure(*result.failure, time.Now()).Retryable) {
				if format == formatResponses {
					result.delivered = writeFrame(event.Raw)
				} else {
					body, _ := json.Marshal(openAIError{Error: openAIErrorDetail{Message: result.SafeError, Type: "server_error", Code: "upstream_error"}})
					result.delivered = writeFrame(append(append([]byte("data: "), body...), '\n', '\n')) && writeFrame([]byte("data: [DONE]\n\n"))
				}
			}
			return finish()
		}
		if streaming {
			if format == formatResponses {
				if !writeFrame(event.Raw) {
					return finish()
				}
			} else {
				for _, chunk := range synthesizer.Feed(event) {
					frame := append(append([]byte("data: "), chunk...), '\n', '\n')
					if !writeFrame(frame) {
						return finish()
					}
				}
			}
			if !event.Terminal {
				continue
			}
			if format == formatChat && !writeFrame([]byte("data: [DONE]\n\n")) {
				return finish()
			}
		} else {
			if !event.Terminal {
				continue
			}
			if (event.Outcome != "completed" && event.Outcome != "incomplete") || len(event.ResponseJSON) == 0 {
				result.failure = &gateway.Failure{Kind: gateway.FailureProtocol}
				result.SafeError, result.code, result.Reason = "the provider stream contained an invalid terminal response", "upstream_protocol_error", "upstream_protocol_error"
				return finish()
			}
			// ResponseJSON is the only retained terminal payload. Its enclosing frame
			// is already bounded by the provider reader's per-frame limit.
			body := event.ResponseJSON
			if format == formatChat {
				translated, err := provider.TranslateChatCompletionResponse(handle.ID(), alias, time.Now().Unix(), body, chatOutput)
				if err != nil {
					result.failure = &gateway.Failure{Kind: gateway.FailureProtocol}
					result.SafeError, result.code, result.Reason = "the provider response could not be translated to the chat format", "upstream_protocol_error", "upstream_protocol_error"
					return finish()
				}
				body = translated
			}
			setFinalUpstreamHeaders(w, result)
			if err := writeBoundedResponse(controller, w, http.StatusOK, "application/json; charset=utf-8", body); err != nil {
				result.Outcome, result.Reason, result.SafeError = "cancelled", "downstream_disconnected", "downstream response could not be delivered"
				result.failure = &gateway.Failure{Kind: gateway.FailureCanceled, ClientCanceled: true}
				return finish()
			}
			delivery = int64Pointer(handle.ElapsedNS())
		}
		result.Outcome, result.Reason, result.SafeError = event.Outcome, event.Outcome, terminalSafeError(event)
		result.failure, result.delivered = nil, true
		return finish()
	}
}

func setFinalUpstreamHeaders(w http.ResponseWriter, result executionResult) {
	w.Header().Del("X-Upstream-Request-ID")
	w.Header().Del("Retry-After")
	if result.UpstreamRequestID != "" {
		w.Header().Set("X-Upstream-Request-ID", result.UpstreamRequestID)
	}
	if result.retryAfter != "" {
		w.Header().Set("Retry-After", result.retryAfter)
	}
}

func (api *harnessAPI) deliverExecution(w *commitmentWriter, r *http.Request, handle *history.Request, result *executionResult, format inferenceFormat) {
	if r.Context().Err() != nil || result.failure != nil && result.failure.ClientCanceled {
		result.Outcome, result.SafeError = "cancelled", "downstream client cancelled the request"
		return
	}
	if result.delivered {
		return
	}
	controller := http.NewResponseController(w)
	var err error
	if w.committed {
		// A committed stream can only end on the same target, never fall back.
		if format == formatResponses {
			if !writeStreamError(controller, w, result.SafeError, handle, &result.Timings.DownstreamDeliveryNS) {
				err = io.ErrClosedPipe
			}
		} else {
			// End chat streams with an error data frame and the [DONE] sentinel.
			body, _ := json.Marshal(openAIError{Error: openAIErrorDetail{Message: result.SafeError, Type: "server_error", Code: "upstream_error"}})
			err = writeDownstreamFrame(controller, w, append(append([]byte("data: "), body...), '\n', '\n'))
			if err == nil {
				err = writeDownstreamFrame(controller, w, []byte("data: [DONE]\n\n"))
			}
		}
	} else {
		setFinalUpstreamHeaders(w, *result)
		if result.capability != nil {
			body, _ := json.Marshal(openAIError{Error: openAIErrorDetail{Message: result.capability.Message, Type: "invalid_request_error", Code: result.capability.Code, Param: result.capability.Param}})
			err = writeBoundedResponse(controller, w, http.StatusBadRequest, "application/json; charset=utf-8", append(body, '\n'))
		} else {
			err = writeBoundedHarnessError(controller, w, result.status, result.SafeError, result.errorType, result.code)
		}
	}
	if err != nil {
		result.Outcome, result.SafeError = "cancelled", "downstream response could not be delivered"
	} else {
		result.Timings.DownstreamDeliveryNS = int64Pointer(handle.ElapsedNS())
	}
}
