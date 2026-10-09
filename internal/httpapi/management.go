// Package httpapi implements the JSON management API and bearer-authenticated
// harness endpoints. The caller applies Host and browser-origin guards.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gosuda/gyemoim/internal/config"
	"github.com/gosuda/gyemoim/internal/gateway"
	"github.com/gosuda/gyemoim/internal/history"
	"github.com/gosuda/gyemoim/internal/siwc"
)

const (
	managementBodyLimit = 1 << 20
	openAIBaseURL       = "https://api.openai.com/v1"
)

type managementAPI struct {
	store    *config.Store
	gateway  *gateway.Service
	fallback http.Handler
	oauth    *siwc.Manager
	history  *history.QueryService
	storage  *history.Recorder
	backoff  loginBackoff
}

// NewManagement creates the management API handler. Paths outside the JSON API
// routes fall through to the embedded UI, which also owns /api/status.
func NewManagement(store *config.Store, fallback http.Handler, oauthManager *siwc.Manager, recorder *history.Recorder) http.Handler {
	return &managementAPI{store: store, gateway: gateway.New(store), fallback: fallback, oauth: oauthManager, history: history.NewQueryService(recorder), storage: recorder}
}

func (api *managementAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		api.fallback.ServeHTTP(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	// Login needs no session; logout accepts stale sessions so browsers can
	// always clean up. Every other management path requires a valid session.
	if len(parts) == 2 && parts[0] == "auth" && parts[1] == "login" {
		api.login(w, r)
		return
	}
	if len(parts) == 2 && parts[0] == "auth" && parts[1] == "logout" {
		api.logout(w, r)
		return
	}
	user, session, ok := api.authenticate(w, r)
	if !ok {
		return
	}
	// Forced password change: only the password change itself and logout (handled
	// above) work until the user sets a new password. /api/auth/me stays reachable
	// so the change-password page can show which user is signed in.
	if user.MustChangePassword && r.URL.Path != "/api/auth/password" && r.URL.Path != "/api/auth/me" {
		writeManagementError(w, http.StatusForbidden, "a password change is required before using the management API", "password_change_required")
		return
	}
	if r.URL.Path == "/api/status" {
		api.fallback.ServeHTTP(w, r)
		return
	}
	switch {
	case len(parts) == 1 && parts[0] == "providers":
		api.providers(w, r)
	case len(parts) == 2 && parts[0] == "providers":
		api.provider(w, r, parts[1])
	case len(parts) == 4 && parts[0] == "providers" && parts[2] == "oauth" && parts[3] == "start":
		api.startProviderOAuth(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "providers" && parts[2] == "disconnect":
		api.disconnectProvider(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "providers" && parts[2] == "models":
		api.providerModels(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "service-accounts":
		api.serviceAccounts(w, r)
	case len(parts) == 2 && parts[0] == "service-accounts":
		api.serviceAccount(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "service-accounts" && parts[2] == "keys":
		api.serviceAccountKeys(w, r, parts[1])
	case len(parts) == 5 && parts[0] == "service-accounts" && parts[2] == "keys" && parts[4] == "revoke":
		api.revokeServiceAccountKey(w, r, parts[1], parts[3])
	case len(parts) == 3 && parts[0] == "service-accounts" && parts[2] == "grants":
		api.serviceAccountGrants(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "service-accounts" && parts[2] == "pi-config":
		api.piAccountConfig(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "models":
		api.models(w, r)
	case len(parts) == 2 && parts[0] == "models":
		api.model(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "requests":
		api.requestList(w, r)
	case len(parts) == 2 && parts[0] == "requests":
		api.requestDetail(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "requests" && parts[2] == "events":
		api.requestEvents(w, r, parts[1])
	case len(parts) == 4 && parts[0] == "requests" && parts[2] == "events":
		api.requestEventContent(w, r, parts[1], parts[3])
	case len(parts) == 3 && parts[0] == "requests" && parts[2] == "body":
		api.requestContent(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "usage":
		api.usage(w, r)
	case len(parts) == 2 && parts[0] == "auth" && parts[1] == "me":
		api.me(w, r, user)
	case len(parts) == 2 && parts[0] == "auth" && parts[1] == "password":
		api.changePassword(w, r, session, user)
	case len(parts) == 1 && parts[0] == "users":
		api.users(w, r)
	case len(parts) == 2 && parts[0] == "users":
		api.user(w, r, parts[1], user)
	case len(parts) == 3 && parts[0] == "users" && (parts[2] == "disable" || parts[2] == "enable"):
		api.setUserEnabled(w, r, parts[1], user, parts[2] == "disable")
	case len(parts) == 3 && parts[0] == "users" && parts[2] == "password":
		api.resetUserPassword(w, r, parts[1])
	case len(parts) == 1 && parts[0] == "storage":
		api.storageStatus(w, r)
	case len(parts) == 2 && parts[0] == "storage" && parts[1] == "delete":
		api.deleteStorageHistory(w, r)
	default:
		writeManagementError(w, http.StatusNotFound, "management endpoint not found", "not_found")
	}
}

func (api *managementAPI) providers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		providers, err := api.store.ListProviders(r.Context())
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, providers)
	case http.MethodPost:
		var input struct {
			Name string  `json:"name"`
			Type *string `json:"type"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			writeManagementError(w, http.StatusBadRequest, "name is required", "invalid_request")
			return
		}
		providerType := "openai"
		if input.Type != nil {
			providerType = strings.TrimSpace(*input.Type)
		}
		if providerType != "openai" {
			writeManagementError(w, http.StatusBadRequest, "unsupported provider type", "invalid_request")
			return
		}
		provider, err := api.store.CreateProvider(r.Context(), config.Provider{
			Name: name, Type: "openai", BaseURL: openAIBaseURL, Status: "disconnected",
		})
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, provider)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (api *managementAPI) startProviderOAuth(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !decodeOptionalEmptyJSON(w, r) {
		return
	}
	if api.oauth == nil {
		writeManagementError(w, http.StatusServiceUnavailable, "OAuth sign-in is unavailable", "service_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	authorizationURL, err := api.oauth.Start(ctx, id)
	if errors.Is(err, config.ErrNotFound) {
		writeManagementFailure(w, err)
		return
	}
	if err != nil {
		writeManagementError(w, http.StatusBadGateway, "Could not start OpenAI sign-in. Try again shortly.", "oauth_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		AuthorizationURL string `json:"authorizationUrl"`
	}{AuthorizationURL: authorizationURL})
}

func (api *managementAPI) disconnectProvider(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !decodeOptionalEmptyJSON(w, r) {
		return
	}
	if api.oauth == nil {
		writeManagementError(w, http.StatusServiceUnavailable, "OAuth sign-out is unavailable", "service_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	attempted, confirmed, err := api.oauth.Disconnect(ctx, id)
	if errors.Is(err, config.ErrNotFound) {
		writeManagementFailure(w, err)
		return
	}
	if err != nil {
		writeManagementError(w, http.StatusInternalServerError, "Could not clear the provider credentials.", "provider_disconnect_failed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RevocationAttempted bool `json:"revocationAttempted"`
		RevocationConfirmed bool `json:"revocationConfirmed"`
	}{RevocationAttempted: attempted, RevocationConfirmed: confirmed})
}

func (api *managementAPI) providerModels(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if api.oauth == nil {
		writeManagementError(w, http.StatusServiceUnavailable, "Provider model catalog is unavailable.", "provider_auth_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	models, err := api.oauth.Models(ctx, id)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		var catalogErr *siwc.ProviderCatalogError
		switch {
		case errors.Is(err, config.ErrNotFound):
			writeManagementFailure(w, err)
		case errors.Is(err, siwc.ErrProviderReauthentication):
			writeManagementError(w, http.StatusUnauthorized, "Reconnect this OpenAI provider before loading its models.", "provider_reauthentication_required")
		case errors.Is(err, siwc.ErrProviderNotReady):
			writeManagementError(w, http.StatusConflict, "Connect an OpenAI account before loading its models.", "provider_not_connected")
		case errors.Is(err, siwc.ErrProviderOAuthClient):
			writeManagementError(w, http.StatusBadGateway, "OpenAI OAuth client configuration needs attention.", "provider_oauth_configuration")
		case errors.Is(err, siwc.ErrProviderAuthUnavailable):
			writeManagementError(w, http.StatusServiceUnavailable, "OpenAI authentication or model catalog is temporarily unavailable.", "provider_auth_unavailable")
		case errors.As(err, &catalogErr):
			writeManagementError(w, http.StatusBadGateway, fmt.Sprintf("OpenAI rejected the model catalog request (HTTP %d). Check the connected account's access.", catalogErr.StatusCode), "provider_catalog_rejected")
		case errors.Is(err, context.DeadlineExceeded):
			writeManagementError(w, http.StatusServiceUnavailable, "OpenAI authentication or model catalog is temporarily unavailable.", "provider_auth_unavailable")
		default:
			writeManagementError(w, http.StatusInternalServerError, "Could not load provider models.", "provider_models_failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, models)
}

func (api *managementAPI) provider(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		provider, err := api.store.GetProvider(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, provider)
	case http.MethodPut:
		var input struct {
			Name string `json:"name"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			writeManagementError(w, http.StatusBadRequest, "name is required", "invalid_request")
			return
		}
		if err := api.store.RenameProvider(r.Context(), id, name); err != nil {
			writeManagementFailure(w, err)
			return
		}
		provider, err := api.store.GetProvider(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, provider)
	case http.MethodDelete:
		if api.oauth == nil {
			writeManagementError(w, http.StatusServiceUnavailable, "Provider deletion is unavailable.", "service_unavailable")
			return
		}
		if err := api.oauth.DeleteProvider(r.Context(), id); err != nil {
			writeManagementFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (api *managementAPI) serviceAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := api.store.ListServiceAccounts(r.Context())
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, accounts)
	case http.MethodPost:
		var input struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" {
			writeManagementError(w, http.StatusBadRequest, "name is required", "invalid_request")
			return
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		account, err := api.store.CreateServiceAccount(r.Context(), config.ServiceAccount{Name: name, Enabled: enabled})
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, account)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (api *managementAPI) serviceAccount(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		account, err := api.store.GetServiceAccount(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, account)
	case http.MethodPut:
		var input struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		name := strings.TrimSpace(input.Name)
		if name == "" || input.Enabled == nil {
			writeManagementError(w, http.StatusBadRequest, "name and enabled are required", "invalid_request")
			return
		}
		account := config.ServiceAccount{ID: id, Name: name, Enabled: *input.Enabled}
		if err := api.store.UpdateServiceAccount(r.Context(), account); err != nil {
			writeManagementFailure(w, err)
			return
		}
		account, err := api.store.GetServiceAccount(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, account)
	case http.MethodDelete:
		if err := api.store.DeleteServiceAccount(r.Context(), id); err != nil {
			writeManagementFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (api *managementAPI) serviceAccountKeys(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method == http.MethodGet {
		if _, err := api.store.GetServiceAccount(r.Context(), accountID); err != nil {
			writeManagementFailure(w, err)
			return
		}
		keys, err := api.store.ListLocalKeys(r.Context(), accountID)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, keys)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	if !decodeOptionalEmptyJSON(w, r) {
		return
	}
	if _, err := api.store.GetServiceAccount(r.Context(), accountID); err != nil {
		writeManagementFailure(w, err)
		return
	}
	plaintext, metadata, err := api.gateway.IssueLocalKey(r.Context(), accountID)
	if err != nil {
		writeManagementFailure(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, struct {
		Key      string          `json:"key"`
		Metadata config.LocalKey `json:"metadata"`
	}{Key: plaintext, Metadata: metadata})
}

func (api *managementAPI) revokeServiceAccountKey(w http.ResponseWriter, r *http.Request, accountID, keyID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !decodeOptionalEmptyJSON(w, r) {
		return
	}
	if _, err := api.store.GetServiceAccount(r.Context(), accountID); err != nil {
		writeManagementFailure(w, err)
		return
	}
	if err := api.store.RevokeLocalKeyForAccount(r.Context(), accountID, keyID); err != nil {
		writeManagementFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *managementAPI) serviceAccountGrants(w http.ResponseWriter, r *http.Request, accountID string) {
	if _, err := api.store.GetServiceAccount(r.Context(), accountID); err != nil {
		writeManagementFailure(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		grants, err := api.store.ListModelGrants(r.Context(), accountID)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		modelIDs := make([]string, 0, len(grants))
		for _, grant := range grants {
			modelIDs = append(modelIDs, grant.ModelID)
		}
		writeJSON(w, http.StatusOK, struct {
			ModelIDs []string `json:"modelIds"`
		}{ModelIDs: modelIDs})
	case http.MethodPut:
		var input struct {
			ModelIDs *[]string `json:"modelIds"`
		}
		if !decodeRequiredJSON(w, r, &input) {
			return
		}
		if input.ModelIDs == nil {
			writeManagementError(w, http.StatusBadRequest, "modelIds is required", "invalid_request")
			return
		}
		if err := api.store.ReplaceModelGrants(r.Context(), accountID, *input.ModelIDs); err != nil {
			writeManagementFailure(w, err)
			return
		}
		grants, err := api.store.ListModelGrants(r.Context(), accountID)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		modelIDs := make([]string, 0, len(grants))
		for _, grant := range grants {
			modelIDs = append(modelIDs, grant.ModelID)
		}
		writeJSON(w, http.StatusOK, struct {
			ModelIDs []string `json:"modelIds"`
		}{ModelIDs: modelIDs})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (api *managementAPI) models(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		models, err := api.store.ListModels(r.Context())
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, models)
	case http.MethodPost:
		input, ok := decodeModelInput(w, r)
		if !ok {
			return
		}
		model, err := api.makeModel(r.Context(), input)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		created, err := api.store.CreateModel(r.Context(), model)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (api *managementAPI) model(w http.ResponseWriter, r *http.Request, id string) {
	switch r.Method {
	case http.MethodGet:
		model, err := api.store.GetModel(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, model)
	case http.MethodPut:
		input, ok := decodeModelInput(w, r)
		if !ok {
			return
		}
		model, err := api.makeModel(r.Context(), input)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		model.ID = id
		if err := api.store.UpdateModel(r.Context(), model); err != nil {
			writeManagementFailure(w, err)
			return
		}
		model, err = api.store.GetModel(r.Context(), id)
		if err != nil {
			writeManagementFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, model)
	case http.MethodDelete:
		if err := api.store.DeleteModel(r.Context(), id); err != nil {
			writeManagementFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

type modelInput struct {
	Name          string          `json:"name"`
	ProviderID    string          `json:"providerId"`
	UpstreamModel string          `json:"upstreamModel"`
	Metadata      json.RawMessage `json:"metadata"`
}

func decodeModelInput(w http.ResponseWriter, r *http.Request) (modelInput, bool) {
	var input modelInput
	if !decodeRequiredJSON(w, r, &input) {
		return modelInput{}, false
	}
	input.Name = strings.TrimSpace(input.Name)
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.UpstreamModel = strings.TrimSpace(input.UpstreamModel)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 128 {
		writeManagementError(w, http.StatusBadRequest, "model name must contain 1 to 128 Unicode characters", "invalid_request")
		return modelInput{}, false
	}
	if input.ProviderID == "" || input.UpstreamModel == "" {
		writeManagementError(w, http.StatusBadRequest, "providerId and upstreamModel are required", "invalid_request")
		return modelInput{}, false
	}
	if message := validatePiMetadata(input.Metadata); message != "" {
		writeManagementError(w, http.StatusBadRequest, message, "invalid_request")
		return modelInput{}, false
	}
	return input, true
}

func (api *managementAPI) makeModel(ctx context.Context, input modelInput) (config.Model, error) {
	if _, err := api.store.GetProvider(ctx, input.ProviderID); err != nil {
		return config.Model{}, err
	}
	return config.Model{
		Name: input.Name, Strategy: "single", StrategyConfigJSON: json.RawMessage(`{}`),
		ProviderID: input.ProviderID, UpstreamModel: input.UpstreamModel,
		MetadataJSON: append(json.RawMessage(nil), input.Metadata...),
	}, nil
}

func decodeRequiredJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	body := http.MaxBytesReader(w, r.Body, managementBodyLimit)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeManagementError(w, http.StatusBadRequest, "request body must contain valid JSON with only supported fields", "invalid_request")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeManagementError(w, http.StatusBadRequest, "request body must contain exactly one JSON value", "invalid_request")
		return false
	}
	return true
}

func decodeOptionalEmptyJSON(w http.ResponseWriter, r *http.Request) bool {
	body := http.MaxBytesReader(w, r.Body, managementBodyLimit)
	data, err := io.ReadAll(body)
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request")
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var empty struct{}
	if err := decoder.Decode(&empty); err != nil {
		writeManagementError(w, http.StatusBadRequest, "request body must be an empty JSON object", "invalid_request")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeManagementError(w, http.StatusBadRequest, "request body must contain exactly one JSON value", "invalid_request")
		return false
	}
	return true
}

func writeManagementFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrNotFound):
		writeManagementError(w, http.StatusNotFound, "resource not found", "not_found")
	case errors.Is(err, config.ErrForbidden):
		writeManagementError(w, http.StatusForbidden, "operation is not permitted", "permission_denied")
	case errors.Is(err, config.ErrConflict), errors.Is(err, config.ErrReferenced):
		writeManagementError(w, http.StatusConflict, "resource conflicts with existing configuration", "conflict")
	default:
		writeManagementError(w, http.StatusInternalServerError, "internal server error", "internal_error")
	}
}

func writeManagementError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}{Error: struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}{Message: message, Type: "invalid_request_error", Code: code}})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeManagementError(w, http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
