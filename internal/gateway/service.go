// Package gateway contains ServiceAccount authentication, route snapshots, and
// the request-local Model selection interface used by the inference handler.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
)

var ErrUnauthenticated = errors.New("service account authentication failed")
var ErrSelectionExhausted = errors.New("routing selection exhausted")
var ErrUnsupportedStrategy = errors.New("routing strategy is unsupported")

// LocalKeyPrefix is the fixed prefix of every locally issued harness key.
const LocalKeyPrefix = "gym_"

// Identity is an admission-time ServiceAccount snapshot paired with the key
// that authenticated it. Storage revalidates that key when resolving a route.
type Identity struct {
	Account config.ServiceAccount
	KeyID   string
}

// Target identifies the Provider and upstream model chosen for one attempt.
type Target struct {
	ProviderID    string `json:"providerId"`
	UpstreamModel string `json:"upstreamModel"`
}

// AttemptOutcome describes an attempt before a strategy considers another one.
type AttemptOutcome struct {
	Outcome           string `json:"outcome"`
	HTTPStatus        int    `json:"httpStatus,omitempty"`
	ResponseCommitted bool   `json:"responseCommitted"`
}

// RoutingRequest is an immutable configuration snapshot plus the incoming body.
// Strategies must keep their mutable selection state in the returned Selection.
type RoutingRequest struct {
	Model      config.Model
	RawRequest json.RawMessage
}

type Selection interface {
	Next(context.Context, *AttemptOutcome) (Target, error)
}

type RoutingStrategy interface {
	NewSelection(context.Context, RoutingRequest) (Selection, error)
}

// SingleTargetStrategy implements the only first-version selection strategy.
// The strategy object itself has no request state; each call gets its own selection.
type SingleTargetStrategy struct{}

func (SingleTargetStrategy) NewSelection(_ context.Context, request RoutingRequest) (Selection, error) {
	if request.Model.Strategy != "single" {
		return nil, ErrUnsupportedStrategy
	}
	if request.Model.ProviderID == "" || request.Model.UpstreamModel == "" {
		return nil, errors.New("single-target model is missing its target")
	}
	return &singleSelection{target: Target{ProviderID: request.Model.ProviderID, UpstreamModel: request.Model.UpstreamModel}}, nil
}

type singleSelection struct {
	target Target
	used   bool
}

func (s *singleSelection) Next(_ context.Context, previous *AttemptOutcome) (Target, error) {
	if previous != nil || s.used {
		return Target{}, ErrSelectionExhausted
	}
	s.used = true
	return s.target, nil
}

// ResolvedRoute combines the current ServiceAccount, Model, Provider, and a
// request-local strategy selection. The storage read transaction has ended before
// this value is returned, so callers must never hold a DB transaction over I/O.
type ResolvedRoute struct {
	Identity  Identity
	Model     config.Model
	Provider  config.Provider
	Selection Selection
}

// Service owns the local-key authentication and configuration routing boundary.
type Service struct {
	store    *config.Store
	strategy RoutingStrategy
	// usage records successful local-key authentications in memory for the
	// periodic last-used flush. Optional and nil-safe; recording here is a
	// constant-time in-memory write and never blocks or touches the database.
	usage *KeyUsageTracker
}

func New(store *config.Store, usage *KeyUsageTracker) *Service {
	return &Service{store: store, strategy: SingleTargetStrategy{}, usage: usage}
}

// AuthenticateBearer accepts exactly an Authorization value of the form
// "Bearer gym_<base64url-encoded 32 random bytes>" and returns its identity snapshot.
func (s *Service) AuthenticateBearer(ctx context.Context, authorization string) (Identity, error) {
	scheme, token, found := strings.Cut(authorization, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Identity{}, ErrUnauthenticated
	}
	if !validLocalKey(token) {
		return Identity{}, ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte(token))
	key, account, err := s.store.LookupLocalKey(ctx, hash[:])
	if errors.Is(err, config.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, fmt.Errorf("authenticate local ServiceAccount key: %w", err)
	}
	// Successful authentication only; failures above return before this
	// constant-time in-memory record (see KeyUsageTracker).
	s.usage.Record(key.ID, time.Now().UTC())
	return Identity{Account: account, KeyID: key.ID}, nil
}

// IssueLocalKey creates a fresh local key with an optional human label. The
// plaintext is returned to the caller exactly once; only its SHA-256 hash and
// final-four-character hint persist.
func (s *Service) IssueLocalKey(ctx context.Context, accountID, label string) (string, config.LocalKey, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", config.LocalKey{}, fmt.Errorf("generate local ServiceAccount key: %w", err)
	}
	plaintext := LocalKeyPrefix + base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(plaintext))
	key, err := s.store.CreateLocalKey(ctx, accountID, hash[:], plaintext[len(plaintext)-4:], label)
	if err != nil {
		return "", config.LocalKey{}, err
	}
	return plaintext, key, nil
}

// ResolveRoute snapshots an authorized model and its current Provider, then
// creates request-local strategy state for the caller's incoming request.
func (s *Service) ResolveRoute(ctx context.Context, identity Identity, modelName string, rawRequest json.RawMessage) (ResolvedRoute, error) {
	account, model, provider, err := s.store.ResolveGrantedRoute(ctx, identity.Account.ID, identity.KeyID, modelName)
	if err != nil {
		if errors.Is(err, config.ErrUnauthorized) {
			return ResolvedRoute{}, ErrUnauthenticated
		}
		return ResolvedRoute{}, err
	}
	routingRequest := RoutingRequest{
		Model:      model,
		RawRequest: append(json.RawMessage(nil), rawRequest...),
	}
	selection, err := s.strategy.NewSelection(ctx, routingRequest)
	if err != nil {
		return ResolvedRoute{}, err
	}
	identity.Account = account
	return ResolvedRoute{Identity: identity, Model: model, Provider: provider, Selection: selection}, nil
}

// ListModels returns the current explicit grants for the authenticated account.
func (s *Service) ListModels(ctx context.Context, identity Identity) ([]config.Model, error) {
	models, err := s.store.ListGrantedModelsForKey(ctx, identity.Account.ID, identity.KeyID)
	if errors.Is(err, config.ErrUnauthorized) {
		return nil, ErrUnauthenticated
	}
	return models, err
}

func validLocalKey(token string) bool {
	if !strings.HasPrefix(token, LocalKeyPrefix) || strings.ContainsAny(token, " \t\r\n") {
		return false
	}
	encoded := strings.TrimPrefix(token, LocalKeyPrefix)
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == encoded
}
