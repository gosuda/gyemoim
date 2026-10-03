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
	AccessToken(context.Context, string) (string, error)
}

type harnessAPI struct {
	gateway  *gateway.Service
	recorder *history.Recorder
	tokens   tokenSource
	adapter  provider.Adapter
	slots    chan struct{}
}

// NewHarness creates the local bearer-authenticated model-list and Responses
// endpoints. The recorder admission fence is required before provider auth or I/O.
func NewHarness(service *gateway.Service, recorder *history.Recorder, tokens tokenSource, adapter provider.Adapter) http.Handler {
	return &harnessAPI{
		gateway: service, recorder: recorder, tokens: tokens, adapter: adapter,
		slots: make(chan struct{}, maxInferenceRequests),
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
		api.serveResponses(w, r)
	default:
		writeHarnessError(w, http.StatusNotFound, "the requested endpoint was not found", "invalid_request_error", "not_found")
	}
}

func (api *harnessAPI) serveModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeHarnessError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	identity, ok := api.authenticate(w, r)
	if !ok {
		return
	}
	models, err := api.gateway.ListModels(r.Context(), identity)
	if err != nil {
		api.writeGatewayError(w, err)
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
		} else {
			writeHarnessError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		}
		return gateway.Identity{}, false
	}
	return identity, true
}

func (api *harnessAPI) serveResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHarnessError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
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
		return
	}
	if api.recorder == nil || api.tokens == nil || api.adapter == nil {
		writeHarnessError(w, http.StatusServiceUnavailable, "request recording or provider service is unavailable", "server_error", "service_unavailable")
		return
	}

	requestController := http.NewResponseController(w)
	if err := requestController.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeHarnessError(w, http.StatusBadRequest, "the request body could not be read", "invalid_request_error", "invalid_request")
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
		} else {
			writeHarnessError(w, http.StatusBadRequest, "the request body could not be read", "invalid_request_error", "invalid_request")
		}
		return
	}
	var fields map[string]json.RawMessage
	if !json.Valid(body) || json.Unmarshal(body, &fields) != nil || fields == nil {
		writeHarnessError(w, http.StatusBadRequest, "the request body must be a JSON object", "invalid_request_error", "invalid_json")
		return
	}
	var modelName string
	if modelRaw, exists := fields["model"]; !exists || json.Unmarshal(modelRaw, &modelName) != nil || strings.TrimSpace(modelName) == "" {
		writeHarnessError(w, http.StatusBadRequest, "the request must include a non-empty model", "invalid_request_error", "model_required")
		return
	}
	route, err := api.gateway.ResolveRoute(r.Context(), identity, modelName, body)
	if err != nil {
		api.writeGatewayError(w, err)
		return
	}

	handle, err := api.recorder.Begin(history.Start{
		ServiceAccount:  history.ServiceAccountSnapshot{ID: route.Identity.Account.ID, Name: route.Identity.Account.Name},
		Model:           history.ModelSnapshot{ID: route.Model.ID, Name: route.Model.Name, Version: route.Model.Version, Strategy: route.Model.Strategy},
		IncomingRequest: body,
	})
	if err != nil {
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
	finish := func(outcome string, status int, safeError string, usage *history.Usage, timings history.Timings, details history.EndDetails) {
		if ended {
			return
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
		message := "the configured model route is unavailable"
		finish("failed", 0, message, nil, history.Timings{}, history.EndDetails{})
		writeHarnessError(w, http.StatusInternalServerError, message, "server_error", "route_unavailable")
		return
	}

	prepared, err := api.adapter.Prepare(selectedTarget.UpstreamModel, body)
	if err != nil {
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
	if !prepared.ClientStream {
		message := "non-streaming Responses are not supported yet; set stream to true"
		finish("failed", 0, message, nil, history.Timings{}, history.EndDetails{})
		writeHarnessError(w, http.StatusBadRequest, message, "invalid_request_error", "stream_required")
		return
	}

	managedToken, err := api.tokens.AccessToken(r.Context(), route.Provider.ID)
	authPreparationNS := handle.ElapsedNS()
	timings := history.Timings{AuthenticationPreparationNS: &authPreparationNS}
	if err != nil {
		if r.Context().Err() != nil {
			finish("cancelled", 0, "request was cancelled during provider authentication", nil, timings, history.EndDetails{})
			return
		}
		message, code := safeProviderAuthError(err)
		finish("failed", 0, message, nil, timings, history.EndDetails{})
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", code)
		return
	}

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
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || upstream.UnexpectedContentType {
			// The body is kept in its own bounded record as exact bytes. It is not
			// echoed to the harness because it may contain arbitrary provider content.
			_ = handle.HTTPResponse(upstream.StatusCode, upstreamRequestID, upstream.ErrorBody, upstream.ErrorBodyTruncated, upstream.ErrorBodyReadFailed)
		}
	}
	if sendErr != nil {
		if r.Context().Err() != nil || errors.Is(sendErr, context.Canceled) {
			finish("cancelled", upstreamStatus, "request was cancelled while contacting the provider", nil, timings, endDetails)
			return
		}
		message := "the provider could not be reached"
		if errors.Is(sendErr, provider.ErrNoEventStream) {
			message = "the provider response was not an event stream"
		}
		finish("failed", upstreamStatus, message, nil, timings, endDetails)
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", "upstream_error")
		return
	}
	if upstream == nil {
		message := "the provider returned no response"
		finish("failed", 0, message, nil, timings, endDetails)
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", "upstream_error")
		return
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 || upstream.UnexpectedContentType {
		message := "the provider returned an error response"
		finish("failed", upstream.StatusCode, message, nil, timings, endDetails)
		writeHarnessError(w, http.StatusBadGateway, message, "server_error", "upstream_error")
		return
	}

	traceSnapshot := trace.Snapshot()
	timings = providerTimings(authPreparationNS, traceSnapshot, nil)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if upstreamRequestID != "" {
		w.Header().Set("X-Upstream-Request-ID", upstreamRequestID)
	}
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	terminalOutcome := ""
	safeEndError := ""
	var downstreamDeliveryNS *int64
	for {
		event, eventErr := upstream.NextEvent()
		if eventErr != nil {
			traceSnapshot = trace.Snapshot()
			timings = providerTimings(authPreparationNS, traceSnapshot, downstreamDeliveryNS)
			if r.Context().Err() != nil || errors.Is(eventErr, context.Canceled) {
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
			if !writeStreamError(controller, w, "the provider event stream could not be read", handle, &downstreamDeliveryNS) {
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

func (api *harnessAPI) writeGatewayError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, gateway.ErrUnauthenticated):
		writeHarnessError(w, http.StatusUnauthorized, "invalid or inactive local API key", "authentication_error", "invalid_api_key")
	case errors.Is(err, config.ErrForbidden):
		writeHarnessError(w, http.StatusForbidden, "model access is not permitted", "permission_error", "model_access_denied")
	case errors.Is(err, config.ErrNotFound):
		writeHarnessError(w, http.StatusNotFound, "the requested model is not available", "invalid_request_error", "model_not_found")
	default:
		writeHarnessError(w, http.StatusInternalServerError, "the request could not be resolved", "server_error", "internal_error")
	}
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
		return "the provider event exceeded the supported size limit"
	case errors.Is(err, provider.ErrInvalidUTF8):
		return "the provider event stream was not valid UTF-8"
	case errors.Is(err, context.Canceled):
		return "the provider event stream was cancelled"
	default:
		return "the provider event stream ended with a read error"
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
