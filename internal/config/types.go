// Package config stores Gyemoim configuration and provider credentials in SQLite.
// Request and usage history belongs in NDJSON files, not this package.
package config

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("configuration record not found")

// Provider is the non-secret, UI-safe part of an upstream connection.
type Provider struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	BaseURL   string    `json:"baseUrl"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ProviderRegistration identifies an OAuth registration and account independently
// from the replaceable token set. It contains no bearer tokens.
type ProviderRegistration struct {
	ProviderID      string `json:"-"`
	IssuedClientID  string `json:"-"`
	VerifiedSubject string `json:"-"`
	Email           string `json:"-"`
}

// ProviderCredentials is deliberately separate from Provider so ordinary provider
// responses cannot accidentally serialize credentials. Every field is excluded from
// JSON serialization as an additional guard against accidental marshaling.
type ProviderCredentials struct {
	ProviderID        string    `json:"-"`
	IssuedClientID    string    `json:"-"`
	VerifiedSubject   string    `json:"-"`
	Email             string    `json:"-"`
	AccessToken       string    `json:"-"`
	RefreshToken      string    `json:"-"`
	IDToken           string    `json:"-"`
	ExpiresAt         time.Time `json:"-"`
	EarliestRefreshAt time.Time `json:"-"`
	Scopes            []string  `json:"-"`
}

// ServiceAccount is a local identity used by harnesses.
type ServiceAccount struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LocalKey contains only stored key metadata. The plaintext key is never persisted.
type LocalKey struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"accountId"`
	DisplayHint string     `json:"displayHint"`
	CreatedAt   time.Time  `json:"createdAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// Model is a named route with one optional, referentially-integral target in the
// initial single-target strategy. StrategyConfig and Metadata remain versioned JSON
// payloads so later strategies can extend configuration without a general ORM.
type Model struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Strategy           string          `json:"strategy"`
	Version            int             `json:"version"`
	StrategyConfigJSON json.RawMessage `json:"strategyConfig"`
	ProviderID         string          `json:"providerId,omitempty"`
	UpstreamModel      string          `json:"upstreamModel,omitempty"`
	MetadataJSON       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt          time.Time       `json:"createdAt"`
	UpdatedAt          time.Time       `json:"updatedAt"`
}

// ModelGrant is an explicit permission from one ServiceAccount to use one Model.
type ModelGrant struct {
	AccountID string    `json:"accountId"`
	ModelID   string    `json:"modelId"`
	CreatedAt time.Time `json:"createdAt"`
}
