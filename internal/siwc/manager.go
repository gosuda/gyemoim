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

// Manager owns bounded, short-lived browser flows. It does not perform discovery
// or network access until Start is called by an explicit user action.
type Manager struct {
	store *config.Store
	port  int

	pendingMu  sync.Mutex
	pending    map[string]pendingFlow
	latest     map[string]string
	generation map[string]uint64

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewManager creates an offline manager for the loopback listener.
func NewManager(store *config.Store, port int) *Manager {
	return &Manager{
		store: store, port: port,
		pending: make(map[string]pendingFlow), latest: make(map[string]string),
		generation: make(map[string]uint64), locks: make(map[string]*sync.Mutex),
	}
}

// Start creates one authorization URL. A provider can have only one active flow;
// a later start invalidates its earlier state. No lock is held while the browser is away.
func (m *Manager) Start(ctx context.Context, providerID string) (string, error) {
	if m == nil || m.store == nil || m.port < 1 || m.port > 65535 || providerID == "" {
		return "", errors.New("OAuth manager is not configured")
	}
	unlock := m.lockProvider(providerID)
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
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/auth/callback", m.port)
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
	flow, err := m.consumePending(state, time.Now())
	if err != nil {
		http.Error(w, "invalid or expired OAuth callback", http.StatusBadRequest)
		return
	}

	// Provider errors take precedence over code processing, but are never echoed.
	if errorValues, hasError := query["error"]; hasError {
		if len(errorValues) == 1 && errorValues[0] == "access_denied" {
			m.redirectResult(w, "authorization_denied")
		} else {
			m.redirectResult(w, "failed")
		}
		return
	}
	code, codeOK := exactlyOne(query, "code")
	callbackClientID, clientIDOK := exactlyOne(query, "client_id")
	if !codeOK || code == "" || !clientIDOK {
		m.redirectResult(w, "failed")
		return
	}
	if flow.newClientID {
		if strings.TrimSpace(callbackClientID) == "" || callbackClientID == initialClientID {
			m.redirectResult(w, "failed")
			return
		}
		flow.clientID = callbackClientID
	} else if _, supplied := query["client_id"]; supplied && callbackClientID != flow.clientID {
		m.redirectResult(w, "failed")
		return
	}

	unlock := m.lockProvider(flow.providerID)
	defer unlock()
	m.pendingMu.Lock()
	isLatest := m.generation[flow.providerID] == flow.generation
	m.pendingMu.Unlock()
	if !isLatest {
		m.redirectResult(w, "failed")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if flow.newClientID {
		// Keep the provider-issued registration even when the subsequent exchange
		// fails, so an invalid_grant does not lose the identifier needed for retry.
		if err := m.store.SaveProviderRegistration(ctx, config.ProviderRegistration{
			ProviderID: flow.providerID, IssuedClientID: flow.clientID,
		}); err != nil {
			m.redirectResult(w, "failed")
			return
		}
	}
	tokens, err := exchangeCode(ctx, flow.client, flow.provider, flow.clientID, flow.redirectURI, code, flow.verifier)
	if err != nil {
		m.redirectResult(w, "failed")
		return
	}
	identity, err := verifyIdentity(ctx, flow.provider, flow.clientID, flow.nonce, tokens.IDToken)
	if err != nil {
		m.redirectResult(w, "failed")
		return
	}
	registration, err := m.store.GetProviderRegistration(ctx, flow.providerID)
	if err != nil {
		m.redirectResult(w, "failed")
		return
	}
	if registration.VerifiedSubject != "" && registration.VerifiedSubject != identity.subject {
		m.redirectResult(w, "failed")
		return
	}
	verifiedEmail := identity.email
	if verifiedEmail == "" {
		// Keep a previously verified address if this validated token omits profile email.
		verifiedEmail = registration.Email
	}
	status := connectionStatus(tokens)
	credentials := config.ProviderCredentials{
		ProviderID: flow.providerID, IssuedClientID: flow.clientID,
		VerifiedSubject: identity.subject, Email: verifiedEmail,
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, ExpiresAt: tokens.ExpiresAt,
		EarliestRefreshAt: tokens.EarliestRefreshAt, Scopes: tokens.Scopes,
	}
	if err := m.store.ReplaceProviderCredentialsWithStatus(ctx, credentials, status); err != nil {
		m.redirectResult(w, "failed")
		return
	}
	m.redirectResult(w, status)
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

func (m *Manager) lockProvider(providerID string) func() {
	m.locksMu.Lock()
	lock := m.locks[providerID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[providerID] = lock
	}
	m.locksMu.Unlock()
	lock.Lock()
	return lock.Unlock
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
	Scope             string          `json:"scope"`
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
	result.Scopes = strings.Fields(result.Scope)
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

func connectionStatus(tokens tokenResponse) string {
	if tokens.Scope == "" {
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
		(request.Method == http.MethodPost && request.URL.Path == tokenPath)
	if !allowed || request.URL.RawQuery != "" || request.URL.Fragment != "" {
		return nil, errEndpoint
	}
	return t.base.RoundTrip(request)
}

func oauthHTTPClient() *http.Client {
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
	return &http.Client{
		Transport: restrictedTransport{base: transport}, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
