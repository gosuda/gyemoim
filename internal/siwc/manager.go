// Package siwc implements the local Sign in with ChatGPT authorization-code flow.
package siwc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gosuda/gyemoim/internal/config"
)

const (
	issuerURL       = "https://auth.openai.com"
	authorizePath   = "/api/accounts/authorize"
	tokenPath       = "/api/accounts/oauth/token"
	jwksPath        = "/.well-known/jwks.json"
	revocationPath  = "/api/accounts/oauth/revoke"
	discoveryPath   = "/.well-known/openid-configuration"
	initialClientID = "dynamic_agent_client"
	agentName       = "Gyemoim"
	scopeRequest    = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	resourceURI     = "https://api.openai.com/v1"
	pendingLifetime = 10 * time.Minute
	maxPending      = 128
	maxHTTPBody     = 1 << 20
)

var (
	errInvalidFlow = errors.New("invalid or expired OAuth flow")
	errEndpoint    = errors.New("unexpected Sign in with ChatGPT endpoint")

	ErrProviderNotReady         = errors.New("provider is not ready for direct inference")
	ErrProviderReauthentication = errors.New("provider must be reauthenticated")
	ErrProviderAuthUnavailable  = errors.New("provider authentication is temporarily unavailable")
	ErrProviderOAuthClient      = errors.New("provider OAuth client configuration is invalid")
	ErrRefreshTokenUnusable     = errors.New("provider refresh token is unusable")
)

type pendingFlow struct {
	state       string
	providerID  string
	clientID    string
	newClientID bool
	verifier    string
	nonce       string
	redirectURI string
	expiresAt   time.Time
	generation  uint64
	provider    *oidc.Provider
	client      *http.Client
}

// Manager owns bounded browser flows and provider credentials. Network access begins
// only after an explicit sign-in, model-catalog, token, or disconnect action.
type Manager struct {
	store *config.Store
	port  int

	pendingMu  sync.Mutex
	pending    map[string]pendingFlow
	latest     map[string]string
	generation map[string]uint64

	locksMu sync.Mutex
	locks   map[string]chan struct{}
}

// AccessTokenPreparation serializes a request's durable admission and token
// resolution with provider credential removal. The bearer remains request-local.
type AccessTokenPreparation interface {
	AccessToken(context.Context) (string, error)
	Close()
}

type accessTokenPreparation struct {
	manager    *Manager
	providerID string
	unlock     func()
	mu         sync.Mutex
	closed     bool
}

// NewManager creates an offline manager for the loopback listener.
func NewManager(store *config.Store, port int) *Manager {
	return &Manager{
		store: store, port: port,
		pending: make(map[string]pendingFlow), latest: make(map[string]string),
		generation: make(map[string]uint64), locks: make(map[string]chan struct{}),
	}
}

// Start creates one authorization URL. A provider can have only one active flow;
// a later start invalidates its earlier state. No lock is held while the browser is away.
func (m *Manager) Start(ctx context.Context, providerID string) (string, error) {
	if m == nil || m.store == nil || m.port < 1 || m.port > 65535 || providerID == "" {
		return "", errors.New("OAuth manager is not configured")
	}
	return m.startFlow(ctx, providerID, m.port)
}

// StartWithCallbackPort behaves exactly like Start but builds the loopback
// redirect URI with the given port instead of the server's own listen port.
// It serves the remote enrollment flow: the enrollment script runs a loopback
// listener on the admin's browser machine, and Sign in with ChatGPT accepts
// only http://127.0.0.1:<port>/auth/callback (fixed scheme, host, and path;
// only the port may vary). A provider can have only one active flow; a later
// start invalidates its earlier state.
func (m *Manager) StartWithCallbackPort(ctx context.Context, providerID string, callbackPort int) (string, error) {
	if callbackPort < 1 || callbackPort > 65535 {
		return "", errors.New("callback port must be between 1 and 65535")
	}
	if m == nil || m.store == nil || providerID == "" {
		return "", errors.New("OAuth manager is not configured")
	}
	return m.startFlow(ctx, providerID, callbackPort)
}

func (m *Manager) startFlow(ctx context.Context, providerID string, callbackPort int) (string, error) {
	unlock, err := m.lockProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	defer unlock()

	providerRecord, err := m.store.GetProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	if providerRecord.Type != "openai" {
		return "", errors.New("provider does not support Sign in with ChatGPT")
	}
	registration, registrationErr := m.store.GetProviderRegistration(ctx, providerID)
	newClientID := errors.Is(registrationErr, config.ErrNotFound)
	if registrationErr != nil && !newClientID {
		return "", registrationErr
	}
	clientID := initialClientID
	var idTokenHint, loginHint string
	if !newClientID {
		clientID = registration.IssuedClientID
		if strings.TrimSpace(clientID) == "" || clientID == initialClientID {
			return "", errors.New("stored OAuth registration is invalid")
		}
		loginHint = registration.Email
		credentials, credentialErr := m.store.GetProviderCredentials(ctx, providerID)
		if credentialErr == nil {
			idTokenHint = credentials.IDToken
		} else if !errors.Is(credentialErr, config.ErrNotFound) {
			return "", credentialErr
		}
	}

	hostID, err := m.store.GetOrCreateHostID(ctx)
	if err != nil {
		return "", err
	}
	client := oauthHTTPClient()
	discoveryContext := oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(discoveryContext, issuerURL)
	if err != nil {
		return "", errors.New("Sign in with ChatGPT discovery failed")
	}
	if err := validateDiscovery(provider); err != nil {
		return "", err
	}

	state, err := randomURLToken(32)
	if err != nil {
		return "", err
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return "", err
	}
	challengeDigest := sha256.Sum256([]byte(verifier))
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/auth/callback", callbackPort)
	authorizationURL, err := buildAuthorizationURL(provider, clientID, hostID, redirectURI, state, nonce,
		base64.RawURLEncoding.EncodeToString(challengeDigest[:]), newClientID, idTokenHint, loginHint)
	if err != nil {
		return "", err
	}

	m.pendingMu.Lock()
	m.expirePendingLocked(time.Now())
	if _, exists := m.latest[providerID]; !exists && len(m.pending) >= maxPending {
		m.pendingMu.Unlock()
		return "", errors.New("too many active OAuth sign-in flows")
	}
	if oldState := m.latest[providerID]; oldState != "" {
		delete(m.pending, oldState)
	}
	m.generation[providerID]++
	flow := pendingFlow{
		state: state, providerID: providerID, clientID: clientID, newClientID: newClientID,
		verifier: verifier, nonce: nonce, redirectURI: redirectURI,
		expiresAt: time.Now().Add(pendingLifetime), generation: m.generation[providerID],
		provider: provider, client: client,
	}
	m.pending[state] = flow
	m.latest[providerID] = state
	m.pendingMu.Unlock()
	return authorizationURL, nil
}

// ServeCallback completes the callback and always returns a fixed local redirect
// or a generic callback error. OAuth query values and upstream errors are never reflected.
func (m *Manager) ServeCallback(w http.ResponseWriter, r *http.Request) {
	setCallbackHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "invalid OAuth callback", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	state, stateOK := exactlyOne(query, "state")
	if !stateOK || state == "" {
		http.Error(w, "invalid or expired OAuth callback", http.StatusBadRequest)
		return
	}
	// Provider errors take precedence over code processing, but are never echoed.
	// The raw value is forwarded so CompleteConnectFlow can distinguish
	// access_denied; a repeated error parameter is flattened to a generic
	// failure exactly as before.
	authorizationError := ""
	if errorValues, hasError := query["error"]; hasError {
		if len(errorValues) == 1 && errorValues[0] == "access_denied" {
			authorizationError = errorValues[0]
		} else {
			authorizationError = "error"
		}
	}
	// A repeated code or client_id parameter flattens to the empty value, which
	// fails the completion path exactly like the previous explicit checks.
	code, _ := exactlyOne(query, "code")
	callbackClientID, _ := exactlyOne(query, "client_id")
	_, clientIDPresent := query["client_id"]
	status, err := m.CompleteConnectFlow(r.Context(), state, code, callbackClientID, clientIDPresent, authorizationError)
	if err != nil {
		http.Error(w, "invalid or expired OAuth callback", http.StatusBadRequest)
		return
	}
	m.redirectResult(w, status)
}

// CompleteConnectFlow runs the shared completion path for one pending flow:
// single-use state consumption, provider lock and generation checks, token
// exchange, ID-token verification, and atomic credential storage. It is the
// post-callback core of ServeCallback and is also driven by the remote
// enrollment endpoint, whose script forwards the provider redirect values.
//
// It returns the final connection status using the oauth_result vocabulary
// (connected, plan_usage_disabled, require_reauthentication, authorization_denied,
// failed). A non-empty authorizationError forwards a provider error redirect:
// "access_denied" maps to the authorization_denied status, any other value fails
// the flow. clientIDPresent reports whether the provider echoed a client_id
// query parameter (required for first-time dynamic registration); a repeated
// parameter arrives as present with an empty callbackClientID and fails.
// errInvalidFlow — unknown, expired, or already consumed state — is the only
// error; the status result is returned even for exchange or storage failures,
// and callers decide how to present each outcome (browser redirect or JSON).
func (m *Manager) CompleteConnectFlow(ctx context.Context, state, authorizationCode, callbackClientID string, clientIDPresent bool, authorizationError string) (status string, err error) {
	if m == nil || m.store == nil || state == "" {
		return "", errInvalidFlow
	}
	flow, err := m.consumePending(state, time.Now())
	if err != nil {
		return "", err
	}
	if authorizationError != "" {
		if authorizationError == "access_denied" {
			return "authorization_denied", nil
		}
		return "failed", nil
	}
	if authorizationCode == "" {
		return "failed", nil
	}
	if flow.newClientID {
		if !clientIDPresent || strings.TrimSpace(callbackClientID) == "" || callbackClientID == initialClientID {
			return "failed", nil
		}
		flow.clientID = callbackClientID
	} else if clientIDPresent && callbackClientID != flow.clientID {
		return "failed", nil
	}

	unlock, lockErr := m.lockProvider(ctx, flow.providerID)
	if lockErr != nil {
		return "failed", nil
	}
	defer unlock()
	m.pendingMu.Lock()
	isLatest := m.generation[flow.providerID] == flow.generation
	m.pendingMu.Unlock()
	if !isLatest {
		return "failed", nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if flow.newClientID {
		// Keep the provider-issued registration even when the subsequent exchange
		// fails, so an invalid_grant does not lose the identifier needed for retry.
		if err := m.store.SaveProviderRegistration(ctx, config.ProviderRegistration{
			ProviderID: flow.providerID, IssuedClientID: flow.clientID,
		}); err != nil {
			return "failed", nil
		}
	}
	tokens, err := exchangeCode(ctx, flow.client, flow.provider, flow.clientID, flow.redirectURI, authorizationCode, flow.verifier)
	if err != nil {
		return "failed", nil
	}
	identity, err := verifyIdentity(ctx, flow.provider, flow.clientID, flow.nonce, tokens.IDToken)
	if err != nil {
		return "failed", nil
	}
	registration, err := m.store.GetProviderRegistration(ctx, flow.providerID)
	if err != nil {
		return "failed", nil
	}
	if registration.VerifiedSubject != "" && registration.VerifiedSubject != identity.subject {
		return "failed", nil
	}
	verifiedEmail := identity.email
	if verifiedEmail == "" {
		// Keep a previously verified address if this validated token omits profile email.
		verifiedEmail = registration.Email
	}
	status = connectionStatus(tokens)
	credentials := config.ProviderCredentials{
		ProviderID: flow.providerID, IssuedClientID: flow.clientID,
		VerifiedSubject: identity.subject, Email: verifiedEmail,
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, ExpiresAt: tokens.ExpiresAt,
		EarliestRefreshAt: tokens.EarliestRefreshAt, Scopes: tokens.Scopes,
	}
	if err := m.store.ReplaceProviderCredentialsWithStatus(ctx, credentials, status); err != nil {
		return "failed", nil
	}
	return status, nil
}

func (m *Manager) consumePending(state string, now time.Time) (pendingFlow, error) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	m.expirePendingLocked(now)
	flow, ok := m.pending[state]
	if !ok || subtle.ConstantTimeCompare([]byte(flow.state), []byte(state)) != 1 || !now.Before(flow.expiresAt) {
		return pendingFlow{}, errInvalidFlow
	}
	delete(m.pending, state)
	if m.latest[flow.providerID] == state {
		delete(m.latest, flow.providerID)
	}
	return flow, nil
}

func (m *Manager) expirePendingLocked(now time.Time) {
	for state, flow := range m.pending {
		if !now.Before(flow.expiresAt) {
			delete(m.pending, state)
			if m.latest[flow.providerID] == state {
				delete(m.latest, flow.providerID)
			}
		}
	}
}

func (m *Manager) lockProvider(ctx context.Context, providerID string) (func(), error) {
	m.locksMu.Lock()
	lock := m.locks[providerID]
	if lock == nil {
		lock = make(chan struct{}, 1)
		lock <- struct{}{}
		m.locks[providerID] = lock
	}
	m.locksMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	}
}

// BeginAccessTokenPreparation takes the provider lock and verifies locally that
// the provider is connected. It does not read credentials or perform network I/O.
// Call AccessToken only after the request's durable history admission succeeds.
func (m *Manager) BeginAccessTokenPreparation(ctx context.Context, providerID string) (AccessTokenPreparation, error) {
	if m == nil || m.store == nil || providerID == "" {
		return nil, ErrProviderNotReady
	}
	unlock, err := m.lockProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	providerRecord, err := m.store.GetProvider(ctx, providerID)
	if err != nil {
		unlock()
		return nil, err
	}
	if err := providerReadinessError(providerRecord); err != nil {
		unlock()
		return nil, err
	}
	return &accessTokenPreparation{manager: m, providerID: providerID, unlock: unlock}, nil
}

func (p *accessTokenPreparation) AccessToken(ctx context.Context) (string, error) {
	if p == nil {
		return "", ErrProviderNotReady
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.manager == nil {
		return "", ErrProviderNotReady
	}
	return p.manager.accessTokenLocked(ctx, p.providerID)
}

func (p *accessTokenPreparation) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	unlock := p.unlock
	p.unlock = nil
	p.mu.Unlock()
	if unlock != nil {
		unlock()
	}
}

// AccessToken returns a currently usable direct-use bearer token for an internal
// caller. It is intentionally not exposed by an HTTP management endpoint.
func (m *Manager) AccessToken(ctx context.Context, providerID string) (string, error) {
	if m == nil || m.store == nil || providerID == "" {
		return "", ErrProviderNotReady
	}
	unlock, err := m.lockProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return m.accessTokenLocked(ctx, providerID)
}

// accessTokenLocked resolves or refreshes a token while the caller holds the
// provider lock. Request preparation leases use it without reacquiring the lock.
func (m *Manager) accessTokenLocked(ctx context.Context, providerID string) (string, error) {
	providerRecord, err := m.store.GetProvider(ctx, providerID)
	if err != nil {
		return "", err
	}
	if err := providerReadinessError(providerRecord); err != nil {
		return "", err
	}
	credentials, err := m.store.GetProviderCredentials(ctx, providerID)
	if err != nil {
		return "", err
	}
	if !hasScope(credentials.Scopes, "chatgpt.tokens.use.direct") || !hasScope(credentials.Scopes, "offline_access") ||
		strings.TrimSpace(credentials.RefreshToken) == "" || strings.TrimSpace(credentials.AccessToken) == "" || credentials.ExpiresAt.IsZero() {
		return "", ErrProviderReauthentication
	}
	if credentials.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return credentials.AccessToken, nil
	}
	refreshed, status, err := m.refresh(ctx, credentials)
	if err != nil {
		if errors.Is(err, ErrRefreshTokenUnusable) {
			clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			clearErr := m.store.ClearProviderCredentials(clearCtx, providerID, "require_reauthentication")
			cancel()
			if clearErr != nil {
				return "", fmt.Errorf("could not clear unusable provider credentials: %w", clearErr)
			}
			return "", ErrProviderReauthentication
		}
		return "", err
	}
	if status == "require_reauthentication" {
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		clearErr := m.store.ClearProviderCredentials(clearCtx, providerID, "require_reauthentication")
		cancel()
		if clearErr != nil {
			return "", fmt.Errorf("could not clear incomplete refreshed credentials: %w", clearErr)
		}
		return "", ErrProviderReauthentication
	}
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	persistErr := m.store.ReplaceProviderCredentialsWithStatus(persistCtx, refreshed, status)
	cancelPersist()
	if persistErr != nil {
		return "", fmt.Errorf("could not save refreshed provider credentials: %w", persistErr)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if status != "connected" {
		return "", ErrProviderNotReady
	}
	return refreshed.AccessToken, nil
}

func providerReadinessError(providerRecord config.Provider) error {
	if providerRecord.Status == "require_reauthentication" {
		return ErrProviderReauthentication
	}
	if providerRecord.Type != "openai" || providerRecord.Status != "connected" {
		return ErrProviderNotReady
	}
	return nil
}

// Disconnect clears local credentials and invalidates any in-flight browser
// authorization before it makes a best-effort remote revocation request.
func (m *Manager) Disconnect(ctx context.Context, providerID string) (attempted, confirmed bool, err error) {
	if m == nil || m.store == nil || providerID == "" {
		return false, false, errors.New("OAuth manager is not configured")
	}
	unlock, err := m.lockProvider(ctx, providerID)
	if err != nil {
		return false, false, err
	}
	defer unlock()

	m.pendingMu.Lock()
	m.generation[providerID]++
	if state := m.latest[providerID]; state != "" {
		delete(m.pending, state)
		delete(m.latest, providerID)
	}
	m.pendingMu.Unlock()

	localCtx, cancelLocal := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelLocal()
	providerRecord, providerErr := m.store.GetProvider(localCtx, providerID)
	if providerErr != nil {
		return false, false, providerErr
	}
	if providerRecord.Type != "openai" {
		return false, false, errors.New("provider does not support Sign in with ChatGPT")
	}
	credentials, credentialsErr := m.store.GetProviderCredentials(localCtx, providerID)
	if credentialsErr != nil && !errors.Is(credentialsErr, config.ErrNotFound) {
		return false, false, credentialsErr
	}
	if clearErr := m.store.DisconnectProvider(localCtx, providerID); clearErr != nil {
		return false, false, clearErr
	}
	if credentialsErr != nil || strings.TrimSpace(credentials.RefreshToken) == "" {
		return false, false, nil
	}
	attempted = true
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	confirmed = revokeRefreshToken(revokeCtx, credentials)
	return attempted, confirmed, nil
}

// DeleteProvider removes the provider and its local credentials while holding
// the same lock used by admission, token refresh, and OAuth callbacks. Pending
// browser authorization is invalidated only after the database deletion succeeds.
func (m *Manager) DeleteProvider(ctx context.Context, providerID string) error {
	if m == nil || m.store == nil || providerID == "" {
		return errors.New("OAuth manager is not configured")
	}
	unlock, err := m.lockProvider(ctx, providerID)
	if err != nil {
		return err
	}
	defer unlock()

	if err := m.store.DeleteProvider(ctx, providerID); err != nil {
		return err
	}
	m.pendingMu.Lock()
	m.generation[providerID]++
	if state := m.latest[providerID]; state != "" {
		delete(m.pending, state)
		delete(m.latest, providerID)
	}
	m.pendingMu.Unlock()
	return nil
}

// CatalogModel contains only the documented model identifier and display label.
type CatalogModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// ProviderCatalogError reports only the upstream status code; response bodies can
// contain diagnostics and are never passed to the management API or logs.
type ProviderCatalogError struct {
	StatusCode int
}

func (e *ProviderCatalogError) Error() string {
	return "OpenAI rejected the model catalog request"
}

// Models fetches the selected connected account's displayable SIWC model catalog.
func (m *Manager) Models(ctx context.Context, providerID string) ([]CatalogModel, error) {
	token, err := m.AccessToken(ctx, providerID)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil, ErrProviderAuthUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := openAIHTTPClient().Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrProviderAuthUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, modelCatalogBodyLimit+1))
	if err != nil || len(body) > modelCatalogBodyLimit {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrProviderAuthUnavailable
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			return nil, ErrProviderAuthUnavailable
		}
		return nil, &ProviderCatalogError{StatusCode: response.StatusCode}
	}
	var payload struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&payload); err != nil {
		return nil, ErrProviderAuthUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrProviderAuthUnavailable
	}
	if payload.Models == nil || len(payload.Models) > 2000 {
		return nil, ErrProviderAuthUnavailable
	}
	models := make([]CatalogModel, 0, len(payload.Models))
	seen := make(map[string]struct{}, len(payload.Models))
	for _, item := range payload.Models {
		id := strings.TrimSpace(item.Slug)
		name := strings.TrimSpace(item.DisplayName)
		if item.Visibility != "list" || id == "" || name == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, CatalogModel{ID: id, DisplayName: name})
	}
	return models, nil
}

const modelCatalogBodyLimit = 2 << 20

func (m *Manager) refresh(ctx context.Context, previous config.ProviderCredentials) (config.ProviderCredentials, string, error) {
	client := oauthHTTPClient()
	provider, err := discoverOIDCProvider(ctx, client)
	if err != nil {
		if ctx.Err() != nil {
			return config.ProviderCredentials{}, "", ctx.Err()
		}
		return config.ProviderCredentials{}, "", ErrProviderAuthUnavailable
	}
	tokens, err := exchangeRefreshToken(ctx, client, provider, previous.IssuedClientID, previous.RefreshToken)
	if err != nil {
		return config.ProviderCredentials{}, "", err
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return config.ProviderCredentials{}, "", ErrRefreshTokenUnusable
	}
	identity := verifiedIdentity{subject: previous.VerifiedSubject, email: previous.Email}
	if strings.TrimSpace(tokens.IDToken) != "" {
		identity, err = verifyRefreshedIdentity(ctx, provider, previous.IssuedClientID, tokens.IDToken)
		if err != nil || identity.subject == "" || identity.subject != previous.VerifiedSubject {
			return config.ProviderCredentials{}, "", ErrRefreshTokenUnusable
		}
		if identity.email == "" {
			identity.email = previous.Email
		}
	} else {
		// Refresh responses may omit an ID token. Keep the last validated identity.
		tokens.IDToken = previous.IDToken
	}
	scopes := previous.Scopes
	if tokens.ScopePresent {
		scopes = strings.Fields(tokens.ScopeValue)
	}
	earliest := tokens.EarliestRefreshAt
	if len(earliest) == 0 {
		earliest = previous.EarliestRefreshAt
	}
	if len(earliest) != 0 && !json.Valid(earliest) {
		return config.ProviderCredentials{}, "", ErrRefreshTokenUnusable
	}
	refreshed := config.ProviderCredentials{
		ProviderID: previous.ProviderID, IssuedClientID: previous.IssuedClientID,
		VerifiedSubject: identity.subject, Email: identity.email,
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, ExpiresAt: tokens.ExpiresAt,
		EarliestRefreshAt: earliest, Scopes: scopes,
	}
	status := connectionStatusFromScopes(scopes, refreshed.RefreshToken)
	return refreshed, status, nil
}

func exchangeRefreshToken(ctx context.Context, client *http.Client, provider *oidc.Provider, clientID, refreshToken string) (tokenResponse, error) {
	var result tokenResponse
	endpoint := provider.Endpoint().TokenURL
	if !validIdentityEndpoint(endpoint, tokenPath) || strings.TrimSpace(clientID) == "" || strings.TrimSpace(refreshToken) == "" {
		return result, ErrProviderOAuthClient
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
		"resource":      {resourceURI},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return result, ErrProviderAuthUnavailable
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrProviderAuthUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBody+1))
	if err != nil || len(body) > maxHTTPBody {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, ErrProviderAuthUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var responseError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &responseError)
		switch responseError.Error {
		case "invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired", "refresh_token_invalidated", "refresh_token_reused":
			return result, ErrRefreshTokenUnusable
		case "invalid_client":
			return result, ErrProviderOAuthClient
		}
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout {
			return result, ErrProviderAuthUnavailable
		}
		return result, ErrProviderAuthUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	if strings.TrimSpace(result.AccessToken) == "" || !strings.EqualFold(strings.TrimSpace(result.TokenType), "Bearer") || !json.Valid(result.EarliestRefreshAt) && len(result.EarliestRefreshAt) != 0 {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	seconds, err := strconv.ParseFloat(result.ExpiresIn.String(), 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	duration := time.Duration(seconds * float64(time.Second))
	if duration <= 0 {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	result.ExpiresAt = time.Now().UTC().Add(duration)
	result.ScopeValue, result.ScopePresent, err = parseScopeField(result.Scope)
	if err != nil {
		return tokenResponse{}, ErrRefreshTokenUnusable
	}
	result.Scopes = strings.Fields(result.ScopeValue)
	return result, nil
}

func verifyRefreshedIdentity(ctx context.Context, provider *oidc.Provider, clientID, rawIDToken string) (verifiedIdentity, error) {
	var identity verifiedIdentity
	idToken, err := provider.Verifier(&oidc.Config{ClientID: clientID}).Verify(ctx, rawIDToken)
	if err != nil || idToken.Subject == "" || idToken.IssuedAt.IsZero() ||
		idToken.IssuedAt.After(time.Now().Add(5*time.Minute)) || idToken.IssuedAt.After(idToken.Expiry) {
		return identity, errors.New("Sign in with ChatGPT returned an invalid refreshed identity token")
	}
	identity.subject = idToken.Subject
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err == nil && claims.EmailVerified {
		identity.email = strings.TrimSpace(claims.Email)
	}
	return identity, nil
}

func connectionStatusFromScopes(scopes []string, refreshToken string) string {
	if !hasScope(scopes, "offline_access") || strings.TrimSpace(refreshToken) == "" {
		return "require_reauthentication"
	}
	if !hasScope(scopes, "chatgpt.tokens.use.direct") {
		return "plan_usage_disabled"
	}
	return "connected"
}

func hasScope(scopes []string, target string) bool {
	for _, scope := range scopes {
		if scope == target {
			return true
		}
	}
	return false
}

func revokeRefreshToken(ctx context.Context, credentials config.ProviderCredentials) bool {
	client := oauthHTTPClient()
	provider, err := discoverOIDCProvider(ctx, client)
	if err != nil {
		return false
	}
	var claims discoveryClaims
	if provider.Claims(&claims) != nil || !validIdentityEndpoint(claims.RevocationEndpoint, revocationPath) {
		return false
	}
	endpoint := claims.RevocationEndpoint
	form := url.Values{
		"token":           {credentials.RefreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {credentials.IssuedClientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBody+1))
	return err == nil && response.StatusCode == http.StatusOK && len(body) == 0
}

type restrictedOpenAITransport struct {
	base http.RoundTripper
}

func (t restrictedOpenAITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Hostname() != "api.openai.com" ||
		request.URL.Port() != "" || request.URL.User != nil || request.URL.Path != "/v1/models" || request.URL.RawQuery != "" || request.URL.Fragment != "" {
		return nil, errEndpoint
	}
	return t.base.RoundTrip(request)
}

var openAIClientOnce sync.Once
var sharedOpenAIClient *http.Client

func openAIHTTPClient() *http.Client {
	openAIClientOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.ForceAttemptHTTP2 = true
		sharedOpenAIClient = &http.Client{
			Transport: restrictedOpenAITransport{base: transport}, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return sharedOpenAIClient
}

func (m *Manager) redirectResult(w http.ResponseWriter, result string) {
	location := "/?" + url.Values{"oauth_result": []string{result}}.Encode()
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

func setCallbackHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'")
}

func exactlyOne(values url.Values, key string) (string, bool) {
	items, exists := values[key]
	if !exists {
		return "", true
	}
	if len(items) != 1 {
		return "", false
	}
	return items[0], true
}

func randomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("could not create secure OAuth request")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

type discoveryClaims struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

func discoverOIDCProvider(ctx context.Context, client *http.Client) (*oidc.Provider, error) {
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuerURL)
	if err != nil {
		return nil, err
	}
	if err := validateDiscovery(provider); err != nil {
		return nil, err
	}
	return provider, nil
}

func validateDiscovery(provider *oidc.Provider) error {
	var claims discoveryClaims
	if err := provider.Claims(&claims); err != nil || claims.Issuer != issuerURL ||
		!validIdentityEndpoint(claims.AuthorizationEndpoint, authorizePath) ||
		!validIdentityEndpoint(claims.TokenEndpoint, tokenPath) ||
		!validIdentityEndpoint(claims.JWKSURI, jwksPath) ||
		!validIdentityEndpoint(claims.RevocationEndpoint, revocationPath) {
		return errEndpoint
	}
	endpoint := provider.Endpoint()
	if endpoint.AuthURL != claims.AuthorizationEndpoint || endpoint.TokenURL != claims.TokenEndpoint {
		return errEndpoint
	}
	return nil
}

func validIdentityEndpoint(raw, expectedPath string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() == "auth.openai.com" &&
		parsed.Port() == "" && parsed.User == nil && parsed.Path == expectedPath &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

func buildAuthorizationURL(provider *oidc.Provider, clientID, hostID, redirectURI, state, nonce, challenge string, first bool, idTokenHint, loginHint string) (string, error) {
	endpoint := provider.Endpoint().AuthURL
	parsed, err := url.Parse(endpoint)
	if err != nil || !validIdentityEndpoint(endpoint, authorizePath) {
		return "", errEndpoint
	}
	values := parsed.Query()
	values.Set("client_id", clientID)
	values.Set("ext_agent_host_id", hostID)
	values.Set("response_type", "code")
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", scopeRequest)
	values.Set("resource", resourceURI)
	values.Set("state", state)
	values.Set("nonce", nonce)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	if first {
		values.Set("agent_name_hint", agentName)
	} else {
		values.Del("agent_name_hint")
		if idTokenHint != "" {
			values.Set("id_token_hint", idTokenHint)
		}
		if loginHint != "" {
			values.Set("login_hint", loginHint)
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

type tokenResponse struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token"`
	IDToken           string          `json:"id_token"`
	TokenType         string          `json:"token_type"`
	Scope             json.RawMessage `json:"scope"`
	ScopeValue        string          `json:"-"`
	ScopePresent      bool            `json:"-"`
	ExpiresIn         json.Number     `json:"expires_in"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
	Scopes            []string        `json:"-"`
	ExpiresAt         time.Time       `json:"-"`
}

func exchangeCode(ctx context.Context, client *http.Client, provider *oidc.Provider, clientID, redirectURI, code, verifier string) (tokenResponse, error) {
	var result tokenResponse
	endpoint := provider.Endpoint().TokenURL
	if !validIdentityEndpoint(endpoint, tokenPath) {
		return result, errEndpoint
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"resource":      {resourceURI},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return result, errors.New("could not prepare OAuth token exchange")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return result, errors.New("Sign in with ChatGPT token exchange failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBody+1))
	if err != nil || len(body) > maxHTTPBody || response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, errors.New("Sign in with ChatGPT token exchange failed")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned an invalid token response")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned an invalid token response")
	}
	if strings.TrimSpace(result.AccessToken) == "" || strings.TrimSpace(result.IDToken) == "" || !strings.EqualFold(strings.TrimSpace(result.TokenType), "Bearer") {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned an incomplete token response")
	}
	if !json.Valid(result.EarliestRefreshAt) && len(result.EarliestRefreshAt) != 0 {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned invalid refresh metadata")
	}
	seconds, err := strconv.ParseFloat(result.ExpiresIn.String(), 64)
	if err != nil || seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned an invalid token lifetime")
	}
	duration := time.Duration(seconds * float64(time.Second))
	if duration <= 0 {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned an invalid token lifetime")
	}
	result.ExpiresAt = time.Now().UTC().Add(duration)
	result.ScopeValue, result.ScopePresent, err = parseScopeField(result.Scope)
	if err != nil {
		return tokenResponse{}, errors.New("Sign in with ChatGPT returned invalid scopes")
	}
	result.Scopes = strings.Fields(result.ScopeValue)
	return result, nil
}

type verifiedIdentity struct {
	subject string
	email   string
}

func verifyIdentity(ctx context.Context, provider *oidc.Provider, clientID, nonce, rawIDToken string) (verifiedIdentity, error) {
	var identity verifiedIdentity
	idToken, err := provider.Verifier(&oidc.Config{ClientID: clientID}).Verify(ctx, rawIDToken)
	if err != nil || idToken.Subject == "" || idToken.Nonce == "" ||
		subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 ||
		idToken.IssuedAt.IsZero() || idToken.IssuedAt.After(time.Now().Add(5*time.Minute)) || idToken.IssuedAt.After(idToken.Expiry) {
		return identity, errors.New("Sign in with ChatGPT returned an invalid identity token")
	}
	identity.subject = idToken.Subject
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idToken.Claims(&claims); err == nil && claims.EmailVerified {
		identity.email = strings.TrimSpace(claims.Email)
	}
	return identity, nil
}

func parseScopeField(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return "", true, errors.New("scope must be a string")
	}
	var scope string
	if err := json.Unmarshal(raw, &scope); err != nil {
		return "", true, err
	}
	return scope, true, nil
}

func connectionStatus(tokens tokenResponse) string {
	if !tokens.ScopePresent || strings.TrimSpace(tokens.ScopeValue) == "" {
		return "plan_usage_disabled"
	}
	hasScope := make(map[string]bool, len(tokens.Scopes))
	for _, scope := range tokens.Scopes {
		hasScope[scope] = true
	}
	if !hasScope["offline_access"] || strings.TrimSpace(tokens.RefreshToken) == "" {
		return "require_reauthentication"
	}
	if !hasScope["chatgpt.tokens.use.direct"] {
		return "plan_usage_disabled"
	}
	return "connected"
}

type restrictedTransport struct {
	base http.RoundTripper
}

func (t restrictedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Hostname() != "auth.openai.com" || request.URL.Port() != "" || request.URL.User != nil {
		return nil, errEndpoint
	}
	allowed := (request.Method == http.MethodGet && (request.URL.Path == discoveryPath || request.URL.Path == jwksPath)) ||
		(request.Method == http.MethodPost && (request.URL.Path == tokenPath || request.URL.Path == revocationPath))
	if !allowed || request.URL.RawQuery != "" || request.URL.Fragment != "" {
		return nil, errEndpoint
	}
	return t.base.RoundTrip(request)
}

var oauthClientOnce sync.Once
var sharedOAuthClient *http.Client

func oauthHTTPClient() *http.Client {
	oauthClientOnce.Do(func() {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.ForceAttemptHTTP2 = true
		sharedOAuthClient = &http.Client{
			Transport: restrictedTransport{base: transport}, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return sharedOAuthClient
}
