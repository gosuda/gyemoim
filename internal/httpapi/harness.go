package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
	"github.com/gosuda/gyemoim/internal/gateway"
)

type harnessAPI struct {
	gateway *gateway.Service
}

// NewHarness creates the local bearer-authenticated OpenAI model-list endpoint.
func NewHarness(service *gateway.Service) http.Handler {
	return &harnessAPI{gateway: service}
}

func (api *harnessAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.URL.Path != "/v1/models" {
		writeHarnessError(w, http.StatusNotFound, "the requested endpoint was not found", "invalid_request_error", "not_found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeHarnessError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}
	identity, err := api.gateway.AuthenticateBearer(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, gateway.ErrUnauthenticated) {
			writeHarnessError(w, http.StatusUnauthorized, "invalid or inactive local API key", "authentication_error", "invalid_api_key")
			return
		}
		writeHarnessError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		return
	}
	models, err := api.gateway.ListModels(r.Context(), identity)
	if err != nil {
		switch {
		case errors.Is(err, gateway.ErrUnauthenticated):
			writeHarnessError(w, http.StatusUnauthorized, "invalid or inactive local API key", "authentication_error", "invalid_api_key")
		case errors.Is(err, config.ErrForbidden):
			writeHarnessError(w, http.StatusForbidden, "model access is not permitted", "permission_error", "model_access_denied")
		default:
			writeHarnessError(w, http.StatusInternalServerError, "internal server error", "server_error", "internal_error")
		}
		return
	}
	data := make([]openAIModel, 0, len(models))
	for _, model := range models {
		data = append(data, openAIModel{ID: model.Name, Object: "model", Created: model.CreatedAt.Unix(), OwnedBy: "gyemoim"})
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openAIModelList{Object: "list", Data: data})
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

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	// Preserve a request identifier header even if the OS random source fails.
	return time.Now().UTC().Format("20060102T150405.000000000")
}
