// Package config stores Gyemoim configuration and provider credentials in SQLite.
// Request and usage history belongs in NDJSON files, not this package.
package config

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("configuration record not found")
var ErrUnauthorized = errors.New("configuration identity is no longer active")
var ErrForbidden = errors.New("configuration permission denied")
var ErrConflict = errors.New("configuration conflict")
var ErrReferenced = errors.New("configuration record is still referenced")

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
	ProviderID        string          `json:"-"`
	IssuedClientID    string          `json:"-"`
	VerifiedSubject   string          `json:"-"`
	Email             string          `json:"-"`
	AccessToken       string          `json:"-"`
	RefreshToken      string          `json:"-"`
	IDToken           string          `json:"-"`
	ExpiresAt         time.Time       `json:"-"`
	EarliestRefreshAt json.RawMessage `json:"-"`
	Scopes            []string        `json:"-"`
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
// Label is an optional, human-chosen display name; an empty value means unlabeled.
// LastUsedAt is flushed periodically from an in-memory tracker (never written on
// the request path), so it lags successful authentications by up to one flush
// interval, and nil means the key has never authenticated successfully.
type LocalKey struct {
	ID          string     `json:"id"`
	AccountID   string     `json:"accountId"`
	DisplayHint string     `json:"displayHint"`
	Label       string     `json:"label,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
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

// User is a management-login identity. PasswordHash holds an already-hashed
// password string (argon2id arrives in a later step); plaintext passwords never
// enter this package, and the hash is excluded from JSON serialization.
// LastLoginAt is stamped transactionally with session creation at successful
// login and is nil when the user has never signed in. ActiveSessionCount is not
// a stored column: the read queries compute it from the sessions table at read
// time (sessions expire lazily, so expiry is re-checked, not row existence) and
// it is never written back.
type User struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	PasswordHash       string     `json:"-"`
	MustChangePassword bool       `json:"mustChangePassword"`
	DisabledAt         *time.Time `json:"disabledAt,omitempty"`
	LastLoginAt        *time.Time `json:"lastLoginAt"`
	ActiveSessionCount int        `json:"activeSessionCount"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// Session is one management login. Like local_keys, only a cryptographic hash of
// the session ID is stored — never the ID itself — so the hash is excluded from
// JSON serialization.
type Session struct {
	IDHash     []byte    `json:"-"`
	UserID     string    `json:"userId"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}
