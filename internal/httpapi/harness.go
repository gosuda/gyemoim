package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
	"github.com/gosuda/gyemoim/internal/gateway"
	"github.com/gosuda/gyemoim/internal/history"
	"github.com/gosuda/gyemoim/internal/provider"
	"github.com/gosuda/gyemoim/internal/siwc"
)

const (
	maxInferenceRequests       = 8
	maxResponseRequestBodySize = 64 << 20
	downstreamWriteTimeout     = 30 * time.Second
)

type tokenSource interface {
	BeginAccessTokenPreparation(context.Context, string) (siwc.AccessTokenPreparation, error)
}

type harnessAPI struct {
	gateway    *gateway.Service
	recorder   *history.Recorder
	tokens     tokenSource
	adapter    provider.Adapter
	slots      chan struct{}
	rejections *RejectionLog
}

// NewHarness creates the local bearer-authenticated model-list and Responses
// endpoints. The recorder admission fence is required before provider auth or I/O.
// The rejection ring is optional diagnostics: it records only pre-admission
// rejections and never influences admission, recording, or inference.
func NewHarness(service *gateway.Service, recorder *history.Recorder, tokens tokenSource, adapter provider.Adapter, rejections *RejectionLog) http.Handler {
	return &harnessAPI{
		gateway: service, recorder: recorder, tokens: tokens, adapter: adapter,
		slots:      make(chan struct{}, maxInferenceRequests),
		rejections: rejections,
	}
}

func (api *harnessAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch r.URL.Path {
	case "/v1/models":
		api.serveModels(w, r)
	case "/v1/responses":
		api.serveInference(w, r, formatResponses)
	case "/v1/chat/completions":
		api.serveInference(w, r, formatChat)
	default:
		writeHarnessError(w, http.StatusNotFound, "the requested endpoint was not found", "invalid_request_error", "not_found")
	}
}

// noteRejection records a pre-admission rejection in the bounded in-memory ring
// (decision 9c). It runs only after the rejection has already been decided and
// the error response is on its way, so it cannot block or gate inference; the
// ring push itself is a constant-time, allocation-free mutex section that never
// touches request recording or history files. The key hint is the last four
// characters of the presented bearer key when it has the local-key shape — the
// same last-4 style the key issuer stores — never the full key. A rejection
// before the key is parsed simply carries an empty hint or identity.
func (api *harnessAPI) noteRejection(r *http.Request, code, model string, identity gateway.Identity) {
	entry := Rejection{At: time.Now().UTC(), Code: code, Model: model}
	if hint := keyHintFromAuthorization(r.Header.Get("Authorization")); hint != "" {
		entry.KeyHint = hint
	}
	if identity.Account.ID != "" {
		entry.ServiceAccountID = identity.Account.ID
		entry.ServiceAccountName = identity.Account.Name
	}
	api.rejections.Record(entry)
}

// keyHintFromAuthorization extracts the last-four-character hint from a
// well-formed local bearer key. It returns "" for absent, malformed, or
// oversized values so arbitrary attacker-controlled bytes are never retained.
func keyHintFromAuthorization(authorization string) string {
	_, token, found := strings.Cut(authorization, " ")
	if !found || !strings.HasPrefix(token, gateway.LocalKeyPrefix) {
		return ""
	}
	if len(token) > len(gateway.LocalKeyPrefix)+128 {
		return ""
	}
	hint := token[len(token)-4:]
	if strings.ContainsAny(hint, " \t\r\n") {
		return ""
	}
	return hint
}

func (api *harnessAPI) serveModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeHarnessError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		api.noteRejection(r, "method_not_allowed", "", gateway.Identity{})
		return
	}
	identity, ok := api.authenticate(w, r)
	if !ok {
		return
	}
	models, err := api.gateway.ListModels(r.Context(), identity)
	if err != nil {
		api.writeGatewayError(w, r, err, identity, "")
		return
	}
	data := make([]openAIModel, 0, len(models))
	for _, model := range models {
		data = append(data, openAIModel{ID: model.Name, Object: "model", Created: model.CreatedAt.Unix(), OwnedBy: "gyemoim"})
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openAIModelList{Object: "list", Data: data})
}

func (api *harnessAPI) authenticate(w http.ResponseWriter, r *http.Request) (gateway.Identity, bool) {
	if api.gateway == nil {
		writeHarnessError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return gateway.Identity{}, false
	}
	identity, err := api.gateway.AuthenticateBearer(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, gateway.ErrUnauthenticated) {
			writeHarnessError(w, http.StatusUnauthorized, "invalid or inactive local API key", "authentication_error", "invalid_api_key")
			api.noteRejection(r, "invalid_api_key", "", gateway.Identity{})
		} else {
			writeHarnessError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
			api.noteRejection(r, "internal_error", "", gateway.Identity{})
		}
		return gateway.Identity{}, false
	}
	return identity, true
}

// inferenceFormat selects the harness-facing request/response format served on
// top of the single Responses upstream path. Both formats share routing,
// admission, recording, limits, and the classified drop policy; only the
// request parsing and the downstream delivery differ.
type inferenceFormat int

const (
	formatResponses inferenceFormat = iota
	formatChat
)

func (api *harnessAPI) serveResponses(w http.ResponseWriter, r *http.Request) {
	api.serveInference(w, r, formatResponses)
}

func (api *harnessAPI) serveChatCompletions(w http.ResponseWriter, r *http.Request) {
	api.serveInference(w, r, formatChat)
}

func (api *harnessAPI) serveInference(w http.ResponseWriter, r *http.Request, format inferenceFormat) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHarnessError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		api.noteRejection(r, "method_not_allowed", "", gateway.Identity{})
		return
	}
	identity, ok := api.authenticate(w, r)
	if !ok {
		return
	}
	// Admission is bounded before body reads, so slow or oversized clients cannot
	// consume unbounded memory or create more than eight concurrent inferences.
	select {
	case api.slots <- struct{}{}:
		defer func() { <-api.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeHarnessError(w, http.StatusServiceUnavailable, "all inference slots are busy; retry shortly", "server_error", "concurrent_request_limit")
		api.noteRejection(r, "concurrent_request_limit", "", identity)
		return
	}
	if api.recorder == nil || api.tokens == nil || api.adapter == nil {
		writeHarnessError(w, http.StatusServiceUnavailable, "request recording or provider service is unavailable", "server_error", "service_unavailable")
		api.noteRejection(r, "service_unavailable", "", identity)
		return
	}

	requestController := http.NewResponseController(w)
	if err := requestController.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeHarnessError(w, http.StatusBadRequest, "the request body could not be read", "invalid_request_error", "invalid_request")
		api.noteRejection(r, "invalid_request", "", identity)
		return
	}
	limitedBody := http.MaxBytesReader(w, r.Body, maxResponseRequestBodySize)
	body, err := io.ReadAll(limitedBody)
	_ = limitedBody.Close()
	_ = requestController.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeHarnessError(w, http.StatusRequestEntityTooLarge, "the request body exceeds the 64 MiB limit", "invalid_request_error", "request_too_large")
			api.noteRejection(r, "request_too_large", "", identity)
		} else {
			writeHarnessError(w, http.StatusBadRequest, "the request body could not be read", "invalid_request_error", "invalid_request")
			api.noteRejection(r, "invalid_request", "", identity)
		}
		return
	}
	var fields map[string]json.RawMessage
	if !json.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		writeHarnessError(w, http.StatusBadRequest, "the request body must be a JSON object", "invalid_request_error", "invalid_json")
		api.noteRejection(r, "invalid_json", "", identity)
		return
	}
	// The chat format is translated up front so the rest of the pipeline —
	// routing, recording, preparation, delivery — runs on one code path. The
	// recorded incoming_request stays the chat body exactly as received.
	var prepareInput json.RawMessage
	var includeChatUsage bool
	if format == formatChat {
		translated, includeUsage, translateErr := provider.TranslateChatCompletions(body)
		if translateErr != nil {
			var capability *provider.CapabilityError
			if errors.As(translateErr, &capability) {
				writeHarnessCapabilityError(w, capability)
				api.noteRejection(r, capability.Code, "", identity)
				return
			}
			writeHarnessError(w, http.StatusBadRequest, "the request could not be translated to the provider request format", "invalid_request_error", "invalid_request")
			api.noteRejection(r, "invalid_request", "", identity)
			return
		}
		prepareInput = translated
		includeChatUsage = includeUsage
	}
	var modelName string
	if modelRaw, exists := fields["model"]; !exists || json.Unmarshal(modelRaw, &modelName) != nil || strings.TrimSpace(modelName) == "" {
		writeHarnessError(w, http.StatusBadRequest, "the request must include a non-empty model", "invalid_request_error", "model_required")
		api.noteRejection(r, "model_required", "", identity)
		return
	}
	route, err := api.gateway.ResolveRoute(r.Context(), identity, modelName, body)
	if err != nil {
		api.writeGatewayError(w, r, err, identity, modelName)
		return
	}
	preparation, err := api.tokens.BeginAccessTokenPreparation(r.Context(), route.Provider.ID)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		message, code := safeProviderAuthError(err)
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", code)
		api.noteRejection(r, code, modelName, route.Identity)
		return
	}
	preparationOpen := true
	releasePreparation := func() {
		if preparationOpen {
			preparation.Close()
			preparationOpen = false
		}
	}
	defer releasePreparation()

	handle, err := api.recorder.Begin(history.Start{
		ServiceAccount:  history.ServiceAccountSnapshot{ID: route.Identity.Account.ID, Name: route.Identity.Account.Name},
		Model:           history.ModelSnapshot{ID: route.Model.ID, Name: route.Model.Name, Version: route.Model.Version, Strategy: route.Model.Strategy},
		IncomingRequest: body,
	})
	if err != nil {
		releasePreparation()
		if errors.Is(err, history.ErrRecordingUnavailable) {
			writeHarnessError(w, http.StatusServiceUnavailable, "request recording is unavailable", "server_error", "recording_unavailable")
		} else {
			writeHarnessError(w, http.StatusInternalServerError, "request could not be recorded", "server_error", "recording_failed")
		}
		return
	}
	// The local ID is the one correlated by the durable request_start record.
	w.Header().Set("X-Request-ID", handle.ID())

	ended := false
	var droppedFields []string
	finish := func(outcome string, status int, safeError string, usage *history.Usage, timings history.Timings, details history.EndDetails) {
		if ended {
			return
		}
		// Classified drops from request preparation are recorded on every end
		// outcome; sites pass provider details and the merge keeps them intact.
		if len(droppedFields) > 0 {
			details.DroppedFields = droppedFields
		}
		_ = handle.EndWithDetails(outcome, status, safeError, usage, timings, details)
		ended = true
	}
	defer func() {
		if !ended {
			finish("failed", 0, "request execution stopped unexpectedly", nil, history.Timings{}, history.EndDetails{})
		}
	}()

	selectedTarget, err := route.Selection.Next(r.Context(), nil)
	if err != nil || selectedTarget.ProviderID != route.Provider.ID || selectedTarget.UpstreamModel != route.Model.UpstreamModel {
		releasePreparation()
		message := "the configured model route is unavailable"
		finish("failed", 0, message, nil, history.Timings{}, history.EndDetails{})
		writeHarnessError(w, http.StatusInternalServerError, message, "server_error", "route_unavailable")
		return
	}

	prepareSource := body
	if format == formatChat {
		prepareSource = prepareInput
	}
	prepared, err := api.adapter.Prepare(selectedTarget.UpstreamModel, prepareSource)
	if err != nil {
		releasePreparation()
		var capability *provider.CapabilityError
		if errors.As(err, &capability) {
			finish("failed", 0, capability.Message, nil, history.Timings{}, history.EndDetails{})
			writeHarnessCapabilityError(w, capability)
			return
		}
		finish("failed", 0, "request could not be prepared for the provider", nil, history.Timings{}, history.EndDetails{})
		writeHarnessError(w, http.StatusBadRequest, "request could not be prepared for the provider", "invalid_request_error", "invalid_request")
		return
	}
	droppedFields = prepared.DroppedFields
	managedToken, err := preparation.AccessToken(r.Context())
	authPreparationNS := handle.ElapsedNS()
	timings := history.Timings{AuthenticationPreparationNS: &authPreparationNS}
	if err != nil {
		releasePreparation()
		if r.Context().Err() != nil {
			finish("cancelled", 0, "request was cancelled during provider authentication", nil, timings, history.EndDetails{})
			return
		}
		message, code := safeProviderAuthError(err)
		finish("failed", 0, message, nil, timings, history.EndDetails{})
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", code)
		return
	}
	releasePreparation()

	target := history.ProviderSnapshot{ID: route.Provider.ID, Name: route.Provider.Name, UpstreamModel: selectedTarget.UpstreamModel}
	if err := handle.Transmit(history.Attempt{Number: 1, Provider: target, EffectiveRequest: prepared.EffectiveJSON}); err != nil {
		// No upstream request is sent if its durable transmission fence fails.
		finish("failed", 0, "request transmission could not be recorded", nil, timings, history.EndDetails{})
		writeHarnessError(w, http.StatusServiceUnavailable, "request recording became unavailable before provider transmission", "server_error", "recording_unavailable")
		return
	}

	trace := provider.NewTraceWithElapsed(handle.ElapsedNS)
	upstream, sendErr := api.adapter.Send(r.Context(), managedToken, handle.ID(), prepared, trace)
	timings = providerTimings(authPreparationNS, trace.Snapshot(), nil)
	if upstream != nil {
		defer upstream.Close()
	}
	var upstreamStatus int
	var upstreamRequestID string
	var upstreamUsage *history.Usage
	var endDetails history.EndDetails
	if upstream != nil {
		upstreamStatus = upstream.StatusCode
		upstreamRequestID = upstream.UpstreamRequestID
		endDetails.UpstreamRequestID = upstreamRequestID
		if upstreamRequestID != "" {
			w.Header().Set("X-Upstream-Request-ID", upstreamRequestID)
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || upstream.UnexpectedContentType {
			// Preserve exact bounded upstream bytes in history; never return the raw
			// body to a caller because it may include provider details or secrets.
			_ = handle.HTTPResponse(upstream.StatusCode, upstream.ContentType, upstreamRequestID, upstream.ErrorBody, upstream.ErrorBodyTruncated, upstream.ErrorBodyReadFailed)
		}
	}
	controller := http.NewResponseController(w)
	if sendErr != nil {
		if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(sendErr, context.Canceled) {
			finish("cancelled", upstreamStatus, "request was cancelled while contacting the provider", nil, timings, endDetails)
			return
		}
		status, message, errorType, code := safeUpstreamSendError(sendErr)
		var downstreamDeliveryNS *int64
		outcome, endError := "failed", message
		if writeBoundedHarnessError(controller, w, status, message, errorType, code) == nil {
			delivery := handle.ElapsedNS()
			downstreamDeliveryNS = &delivery
		} else {
			outcome, endError = "cancelled", "downstream response could not be delivered"
		}
		timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
		finish(outcome, upstreamStatus, endError, nil, timings, endDetails)
		return
	}
	if upstream == nil {
		message := "the provider returned no response"
		var downstreamDeliveryNS *int64
		outcome, endError := "failed", message
		if writeBoundedHarnessError(controller, w, http.StatusBadGateway, message, "server_error", "upstream_error") == nil {
			delivery := handle.ElapsedNS()
			downstreamDeliveryNS = &delivery
		} else {
			outcome, endError = "cancelled", "downstream response could not be delivered"
		}
		timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
		finish(outcome, 0, endError, nil, timings, endDetails)
		return
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || upstream.UnexpectedContentType {
		status, message, errorType, code := safeUpstreamHTTPError(upstream.StatusCode, upstream.UnexpectedContentType)
		if upstream.StatusCode == http.StatusTooManyRequests && upstream.RetryAfter != "" {
			w.Header().Set("Retry-After", upstream.RetryAfter)
		}
		var downstreamDeliveryNS *int64
		outcome, endError := "failed", message
		if writeBoundedHarnessError(controller, w, status, message, errorType, code) == nil {
			delivery := handle.ElapsedNS()
			downstreamDeliveryNS = &delivery
		} else {
			outcome, endError = "cancelled", "downstream response could not be delivered"
		}
		timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
		finish(outcome, upstream.StatusCode, endError, nil, timings, endDetails)
		return
	}

	if prepared.ClientStream && format == formatChat {
		// Chat streaming synthesis is implemented with the chat delivery work;
		// until then the stream request is rejected explicitly instead of
		// returning Responses-format events a chat client cannot parse.
		timings = providerTimings(authPreparationNS, trace.Snapshot(), nil)
		finish("failed", 0, "chat completions streaming is not supported yet", nil, timings, endDetails)
		writeHarnessError(w, http.StatusBadRequest, "chat completions streaming is not supported yet; use stream:false", "invalid_request_error", "unsupported_value")
		return
	}
	if !prepared.ClientStream {
		api.serveNonStreamingResponse(w, r, handle, upstream, trace, authPreparationNS, endDetails, finish, format, route.Model.Name)
		return
	}

	if format == formatChat {
		api.serveChatStreamingResponse(w, r, handle, upstream, trace, authPreparationNS, endDetails, finish, route.Model.Name, includeChatUsage)
		return
	}

	traceSnapshot := trace.Snapshot()
	timings = providerTimings(authPreparationNS, traceSnapshot, nil)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller = http.NewResponseController(w)
	terminalOutcome := ""
	safeEndError := ""
	var downstreamDeliveryNS *int64
	for {
		event, eventErr := upstream.NextEvent()
		if eventErr != nil {
			traceSnapshot = trace.Snapshot()
			timings = providerTimings(authPreparationNS, traceSnapshot, downstreamDeliveryNS)
			if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(eventErr, context.Canceled) {
				finish("cancelled", upstream.StatusCode, "downstream client cancelled the request", upstreamUsage, timings, endDetails)
				return
			}
			if errors.Is(eventErr, io.EOF) {
				safeEndError = "provider stream ended without a terminal response event"
				if !writeStreamError(controller, w, safeEndError, handle, &downstreamDeliveryNS) {
					timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
					finish("cancelled", upstream.StatusCode, "downstream client disconnected before the response was delivered", upstreamUsage, timings, endDetails)
					return
				}
				timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
				finish("incomplete", upstream.StatusCode, safeEndError, upstreamUsage, timings, endDetails)
				return
			}
			safeEndError = safeStreamReadError(eventErr)
			if !writeStreamError(controller, w, safeEndError, handle, &downstreamDeliveryNS) {
				timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
				finish("cancelled", upstream.StatusCode, "downstream client disconnected before the response was delivered", upstreamUsage, timings, endDetails)
				return
			}
			timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish("failed", upstream.StatusCode, safeEndError, upstreamUsage, timings, endDetails)
			return
		}
		if err := handle.Event(event.Raw, event.Name); err != nil {
			// A history failure after admission is counted by the recorder; it must
			// not interrupt another admitted inference or this live client stream.
		}
		if terminalProtocolMismatch(event) {
			message := "the provider stream contained a mismatched terminal response"
			outcome := "failed"
			if !writeStreamError(controller, w, message, handle, &downstreamDeliveryNS) {
				outcome, message = "cancelled", "downstream response could not be delivered"
			}
			timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish(outcome, upstream.StatusCode, message, upstreamUsage, timings, endDetails)
			return
		}
		if event.Terminal {
			terminalOutcome = event.Outcome
			if event.Usage != nil {
				upstreamUsage = usageToHistory(event.Usage)
			}
		}
		if !event.Complete {
			// Keep a final unterminated frame in history, but do not append it to
			// the downstream SSE stream where it could corrupt the next event.
			continue
		}
		if err := writeDownstreamFrame(controller, w, event.Raw); err != nil {
			traceSnapshot = trace.Snapshot()
			timings = providerTimings(authPreparationNS, traceSnapshot, downstreamDeliveryNS)
			finish("cancelled", upstream.StatusCode, "downstream client disconnected before the response was delivered", upstreamUsage, timings, endDetails)
			return
		}
		delivery := handle.ElapsedNS()
		downstreamDeliveryNS = &delivery
		if event.Terminal {
			traceSnapshot = trace.Snapshot()
			timings = providerTimings(authPreparationNS, traceSnapshot, downstreamDeliveryNS)
			finish(terminalOutcome, upstream.StatusCode, terminalSafeError(event), upstreamUsage, timings, endDetails)
			return
		}
	}
}

func (api *harnessAPI) serveNonStreamingResponse(
	w http.ResponseWriter,
	r *http.Request,
	handle *history.Request,
	upstream *provider.UpstreamResponse,
	trace *provider.Trace,
	authPreparationNS int64,
	endDetails history.EndDetails,
	finish func(string, int, string, *history.Usage, history.Timings, history.EndDetails),
	format inferenceFormat,
	modelAlias string,
) {
	controller := http.NewResponseController(w)
	var usage *history.Usage

	writeErrorAndFinish := func(outcome string, status int, message, errorType, code string) {
		var deliveryNS *int64
		endError := message
		if writeBoundedHarnessError(controller, w, status, message, errorType, code) == nil {
			delivery := handle.ElapsedNS()
			deliveryNS = &delivery
		} else {
			outcome, endError = "cancelled", "downstream response could not be delivered"
		}
		timings := providerTimings(authPreparationNS, trace.Snapshot(), deliveryNS)
		finish(outcome, upstream.StatusCode, endError, usage, timings, endDetails)
	}

	for {
		event, eventErr := upstream.NextEvent()
		if eventErr != nil {
			if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(eventErr, context.Canceled) {
				timings := providerTimings(authPreparationNS, trace.Snapshot(), nil)
				finish("cancelled", upstream.StatusCode, "downstream client cancelled the request", usage, timings, endDetails)
				return
			}
			if errors.Is(eventErr, io.EOF) {
				writeErrorAndFinish("incomplete", http.StatusBadGateway,
					"the provider stream ended before a terminal response", "server_error", "upstream_protocol_error")
				return
			}
			status, message, errorType, code := safeUpstreamStreamError(eventErr)
			writeErrorAndFinish("failed", status, message, errorType, code)
			return
		}

		// Record each raw frame before interpreting it. The loop keeps no event
		// history in memory, so long non-streaming responses remain bounded.
		_ = handle.Event(event.Raw, event.Name)
		if event.Terminal && event.Usage != nil {
			usage = usageToHistory(event.Usage)
		}
		if terminalProtocolMismatch(event) {
			writeErrorAndFinish("failed", http.StatusBadGateway,
				"the provider stream contained a mismatched terminal response", "server_error", "upstream_protocol_error")
			return
		}
		if !event.Complete || !event.Terminal {
			continue
		}
		if event.Outcome == "failed" {
			writeErrorAndFinish("failed", http.StatusBadGateway,
				"the provider reported a failed response", "server_error", "provider_response_failed")
			return
		}
		if (event.Outcome != "completed" && event.Outcome != "incomplete") || len(event.ResponseJSON) == 0 {
			writeErrorAndFinish("failed", http.StatusBadGateway,
				"the provider stream contained an invalid terminal response", "server_error", "upstream_protocol_error")
			return
		}

		// ResponseJSON is the only retained terminal payload. Its enclosing frame
		// is already bounded by the provider reader's per-frame limit.
		responseBody := event.ResponseJSON
		if format == formatChat {
			translated, translateErr := provider.TranslateChatCompletionResponse(handle.ID(), modelAlias, time.Now().Unix(), event.ResponseJSON)
			if translateErr != nil {
				writeErrorAndFinish("failed", http.StatusBadGateway,
					"the provider response could not be translated to the chat format", "server_error", "upstream_protocol_error")
				return
			}
			responseBody = translated
		}
		controller = http.NewResponseController(w)
		if err := writeBoundedResponse(controller, w, http.StatusOK, "application/json; charset=utf-8", responseBody); err != nil {
			timings := providerTimings(authPreparationNS, trace.Snapshot(), nil)
			finish("cancelled", upstream.StatusCode, "downstream client disconnected before the response was delivered", usage, timings, endDetails)
			return
		}
		delivery := handle.ElapsedNS()
		timings := providerTimings(authPreparationNS, trace.Snapshot(), &delivery)
		finish(event.Outcome, upstream.StatusCode, terminalSafeError(event), usage, timings, endDetails)
		return
	}
}

// serveChatStreamingResponse delivers a chat completions stream by synthesizing
// chat completion chunks from the upstream Responses event stream. Raw upstream
// frames are still recorded in history; only the downstream chunks are
// synthesized and they are never recorded.
func (api *harnessAPI) serveChatStreamingResponse(
	w http.ResponseWriter,
	r *http.Request,
	handle *history.Request,
	upstream *provider.UpstreamResponse,
	trace *provider.Trace,
	authPreparationNS int64,
	endDetails history.EndDetails,
	finish func(string, int, string, *history.Usage, history.Timings, history.EndDetails),
	modelAlias string,
	includeUsage bool,
) {
	synthesizer := provider.NewChatStreamSynthesizer(handle.ID(), modelAlias, time.Now().Unix(), includeUsage)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)

	var upstreamUsage *history.Usage
	var downstreamDeliveryNS *int64
	markDelivered := func() {
		value := handle.ElapsedNS()
		downstreamDeliveryNS = &value
	}
	// writeChatErrorChunks ends the stream with one chat-format error data
	// frame followed by the [DONE] sentinel, mirroring the Responses path's
	// safe error event for a stream that cannot continue.
	writeChatErrorChunks := func(message string) bool {
		data, _ := json.Marshal(map[string]any{
			"error": openAIErrorDetail{Message: message, Type: "server_error", Code: "upstream_error"},
		})
		frame := append([]byte("data: "), data...)
		frame = append(frame, '\n', '\n')
		if err := writeDownstreamFrame(controller, w, frame); err != nil {
			return false
		}
		if err := writeDownstreamFrame(controller, w, []byte("data: [DONE]\n\n")); err != nil {
			return false
		}
		markDelivered()
		return true
	}
	failCancelled := func(status int, usage *history.Usage) {
		timings := providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
		finish("cancelled", status, "downstream client disconnected before the response was delivered", usage, timings, endDetails)
	}

	for {
		event, eventErr := upstream.NextEvent()
		if eventErr != nil {
			timings := providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			if errors.Is(r.Context().Err(), context.Canceled) || errors.Is(eventErr, context.Canceled) {
				finish("cancelled", upstream.StatusCode, "downstream client cancelled the request", upstreamUsage, timings, endDetails)
				return
			}
			if errors.Is(eventErr, io.EOF) {
				message := "the provider stream ended without a terminal response event"
				if !writeChatErrorChunks(message) {
					failCancelled(upstream.StatusCode, upstreamUsage)
					return
				}
				timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
				finish("incomplete", upstream.StatusCode, message, upstreamUsage, timings, endDetails)
				return
			}
			message := safeStreamReadError(eventErr)
			if !writeChatErrorChunks(message) {
				failCancelled(upstream.StatusCode, upstreamUsage)
				return
			}
			timings = providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish("failed", upstream.StatusCode, message, upstreamUsage, timings, endDetails)
			return
		}
		// Record each raw upstream frame before interpreting it; response_event
		// records stay upstream-native on the chat path.
		_ = handle.Event(event.Raw, event.Name)
		if event.Terminal && event.Usage != nil {
			upstreamUsage = usageToHistory(event.Usage)
		}
		if terminalProtocolMismatch(event) {
			message := "the provider stream contained a mismatched terminal response"
			if !writeChatErrorChunks(message) {
				failCancelled(upstream.StatusCode, upstreamUsage)
				return
			}
			timings := providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish("failed", upstream.StatusCode, message, upstreamUsage, timings, endDetails)
			return
		}
		if event.Terminal && event.Outcome == "failed" {
			message := "the provider reported a failed response"
			if !writeChatErrorChunks(message) {
				failCancelled(upstream.StatusCode, upstreamUsage)
				return
			}
			timings := providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish("failed", upstream.StatusCode, message, upstreamUsage, timings, endDetails)
			return
		}
		if !event.Complete {
			// A final unterminated frame stays in history but feeds no chunk.
			continue
		}
		for _, chunk := range synthesizer.Feed(event) {
			frame := append([]byte("data: "), chunk...)
			frame = append(frame, '\n', '\n')
			if err := writeDownstreamFrame(controller, w, frame); err != nil {
				failCancelled(upstream.StatusCode, upstreamUsage)
				return
			}
			markDelivered()
		}
		if event.Terminal {
			if err := writeDownstreamFrame(controller, w, []byte("data: [DONE]\n\n")); err != nil {
				failCancelled(upstream.StatusCode, upstreamUsage)
				return
			}
			markDelivered()
			timings := providerTimings(authPreparationNS, trace.Snapshot(), downstreamDeliveryNS)
			finish(event.Outcome, upstream.StatusCode, terminalSafeError(event), upstreamUsage, timings, endDetails)
			return
		}
	}
}

func terminalProtocolMismatch(event provider.SSEEvent) bool {
	if event.JSONTypeMismatch {
		return true
	}
	if !event.Complete {
		return false
	}
	switch event.Type {
	case "response.completed", "response.failed", "response.incomplete":
		return !event.Terminal
	default:
		return false
	}
}

func safeUpstreamHTTPError(status int, unexpectedContentType bool) (int, string, string, string) {
	if unexpectedContentType {
		return http.StatusBadGateway, "the provider returned a successful response that was not an event stream", "server_error", "upstream_protocol_error"
	}
	switch status {
	case http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "the configured provider is rate limited", "rate_limit_error", "rate_limit_exceeded"
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway, "the configured provider rejected its authorization", "server_error", "provider_authorization_error"
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		return status, "the configured provider rejected the request", "invalid_request_error", "provider_request_rejected"
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return http.StatusServiceUnavailable, "the configured provider is temporarily unavailable", "server_error", "provider_unavailable"
	default:
		return http.StatusBadGateway, "the configured provider returned an error response", "server_error", "upstream_error"
	}
}

func safeUpstreamSendError(err error) (int, string, string, string) {
	switch {
	case errors.Is(err, provider.ErrNoEventStream):
		return http.StatusBadGateway, "the provider returned a successful response that was not an event stream", "server_error", "upstream_protocol_error"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "the provider request timed out", "server_error", "upstream_timeout"
	case errors.Is(err, provider.ErrUpstreamUnavailable):
		return http.StatusServiceUnavailable, "the provider is temporarily unavailable", "server_error", "provider_unavailable"
	default:
		return http.StatusBadGateway, "the provider could not be reached", "server_error", "upstream_error"
	}
}

func safeUpstreamStreamError(err error) (int, string, string, string) {
	switch {
	case errors.Is(err, provider.ErrEventTooLarge):
		return http.StatusBadGateway, "a provider stream frame exceeded the 64 MiB limit", "server_error", "upstream_frame_too_large"
	case errors.Is(err, provider.ErrInvalidUTF8):
		return http.StatusBadGateway, "the provider stream contained invalid UTF-8", "server_error", "upstream_protocol_error"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "the provider stream read timed out", "server_error", "upstream_timeout"
	default:
		return http.StatusBadGateway, "the provider stream could not be read", "server_error", "upstream_read_error"
	}
}

func writeBoundedHarnessError(controller *http.ResponseController, w http.ResponseWriter, status int, message, errorType, code string) error {
	body, err := json.Marshal(openAIError{Error: openAIErrorDetail{Message: message, Type: errorType, Code: code}})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeBoundedResponse(controller, w, status, "application/json; charset=utf-8", body)
}

func writeBoundedResponse(controller *http.ResponseController, w http.ResponseWriter, status int, contentType string, body []byte) error {
	if err := controller.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout)); err != nil {
		return err
	}
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	return controller.Flush()
}

func (api *harnessAPI) writeGatewayError(w http.ResponseWriter, r *http.Request, err error, identity gateway.Identity, model string) {
	var code string
	switch {
	case errors.Is(err, gateway.ErrUnauthenticated):
		code = "invalid_api_key"
		writeHarnessError(w, http.StatusUnauthorized, "invalid or inactive local API key", "authentication_error", code)
	case errors.Is(err, config.ErrForbidden):
		code = "model_access_denied"
		writeHarnessError(w, http.StatusForbidden, "model access is not permitted", "permission_error", code)
	case errors.Is(err, config.ErrNotFound):
		code = "model_not_found"
		writeHarnessError(w, http.StatusNotFound, "the requested model is not available", "invalid_request_error", code)
	default:
		code = "internal_error"
		writeHarnessError(w, http.StatusInternalServerError, "the request could not be resolved", "server_error", code)
	}
	// Every gateway error here is a pre-admission rejection: the route
	// resolution runs before recorder.Begin on both harness endpoints.
	api.noteRejection(r, code, model, identity)
}

func safeProviderAuthError(err error) (string, string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "provider authentication was cancelled", "provider_auth_cancelled"
	case errors.Is(err, siwc.ErrProviderReauthentication):
		return "the provider must be reconnected before it can serve requests", "provider_reauthentication_required"
	case errors.Is(err, siwc.ErrProviderAuthUnavailable):
		return "provider authentication is temporarily unavailable", "provider_auth_unavailable"
	case errors.Is(err, siwc.ErrProviderOAuthClient):
		return "provider authentication is not configured correctly", "provider_auth_configuration_error"
	case errors.Is(err, siwc.ErrProviderNotReady):
		return "provider authentication is not ready", "provider_not_ready"
	default:
		return "provider authentication is not ready", "provider_not_ready"
	}
}

func safeStreamReadError(err error) string {
	switch {
	case errors.Is(err, provider.ErrEventTooLarge):
		return "a provider stream frame exceeded the 64 MiB limit"
	case errors.Is(err, provider.ErrInvalidUTF8):
		return "the provider stream contained invalid UTF-8"
	case errors.Is(err, context.Canceled):
		return "the provider stream was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the provider stream read timed out"
	default:
		return "the provider stream could not be read"
	}
}

func terminalSafeError(event provider.SSEEvent) string {
	switch event.Outcome {
	case "failed":
		return "the provider reported a failed response"
	case "incomplete":
		return "the provider reported an incomplete response"
	default:
		return ""
	}
}

func usageToHistory(usage *provider.Usage) *history.Usage {
	if usage == nil {
		return nil
	}
	return &history.Usage{
		InputTokens: cloneCount(usage.InputTokens), OutputTokens: cloneCount(usage.OutputTokens),
		CachedInputTokens: cloneCount(usage.CachedInputTokens), ReasoningOutputTokens: cloneCount(usage.ReasoningOutputTokens),
	}
}

func cloneCount(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func providerTimings(authPreparationNS int64, snapshot provider.TimingSnapshot, downstream *int64) history.Timings {
	return history.Timings{
		AuthenticationPreparationNS: int64Pointer(authPreparationNS),
		ConnectionRequestedNS:       cloneCount(snapshot.ConnectionRequestedNS),
		ConnectionEstablishedNS:     cloneCount(snapshot.ConnectionEstablishedNS),
		RequestTransmissionNS:       cloneCount(snapshot.RequestTransmissionNS),
		FirstEventNS:                cloneCount(snapshot.FirstEventNS),
		FirstOutputNS:               cloneCount(snapshot.FirstOutputNS),
		StreamCompletionNS:          cloneCount(snapshot.StreamCompletionNS),
		DownstreamDeliveryNS:        cloneCount(downstream),
		ConnectionReused:            cloneBool(snapshot.ConnectionReused),
	}
}

func int64Pointer(value int64) *int64 { return &value }

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func writeDownstreamFrame(controller *http.ResponseController, w http.ResponseWriter, frame []byte) error {
	if err := controller.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout)); err != nil {
		return err
	}
	defer controller.SetWriteDeadline(time.Time{})
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return controller.Flush()
}

func writeStreamError(controller *http.ResponseController, w http.ResponseWriter, message string, handle *history.Request, delivery **int64) bool {
	data, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": openAIErrorDetail{Message: message, Type: "server_error", Code: "upstream_error"},
	})
	frame := append([]byte("event: error\ndata: "), data...)
	frame = append(frame, '\n', '\n')
	if err := writeDownstreamFrame(controller, w, frame); err != nil {
		return false
	}
	value := handle.ElapsedNS()
	*delivery = &value
	return true
}

type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openAIError struct {
	Error openAIErrorDetail `json:"error"`
}

type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
}

func writeHarnessError(w http.ResponseWriter, status int, message, errorType, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorDetail{Message: message, Type: errorType, Code: code}})
}

func writeHarnessCapabilityError(w http.ResponseWriter, capability *provider.CapabilityError) {
	if capability == nil {
		writeHarnessError(w, http.StatusBadRequest, "request capability is unsupported", "invalid_request_error", "unsupported_parameter")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(openAIError{Error: openAIErrorDetail{
		Message: capability.Message, Type: "invalid_request_error", Code: capability.Code, Param: capability.Param,
	}})
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	// Preserve a request identifier header even if the OS random source fails.
	return time.Now().UTC().Format("20060102T150405.000000000")
}
