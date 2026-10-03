package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const currentSchemaVersion = 1

// Store owns the configuration database. A single connection makes connection-local
// SQLite settings consistent, while the DSN reapplies them if database/sql reconnects.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates or opens dataDir/config.db, applies the SQLite connection settings,
// migrates atomically, and ensures the stable host ID exists before returning.
func Open(ctx context.Context, dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "config.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create or open configuration database %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("set configuration database permissions on %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close configuration database file %q: %w", path, err)
	}

	dsnURL := url.URL{
		Scheme: "file",
		Path:   path,
		RawQuery: "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&" +
			"_pragma=journal_mode(WAL)",
	}
	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite configuration database %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	store := &Store{db: db, path: path}
	closeOnError := func(openErr error) (*Store, error) {
		_ = db.Close()
		return nil, openErr
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("connect to SQLite configuration database %q: %w", path, err))
	}
	if err := store.migrate(ctx); err != nil {
		return closeOnError(err)
	}
	if _, err := store.GetOrCreateHostID(ctx); err != nil {
		return closeOnError(fmt.Errorf("initialize stable host ID: %w", err))
	}
	return store, nil
}

// Close releases the database connection. Callers should do this before releasing
// the process lock for the data directory.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close SQLite configuration database %q: %w", s.path, err)
	}
	return nil
}

// Ping reports whether the SQLite connection is currently usable without exposing
// database errors or stored values to the status endpoint.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("configuration database is closed")
	}
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read configuration schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("configuration database schema version %d is newer than this Gyemoim version (maximum supported: %d)", version, currentSchemaVersion)
	}
	if version == currentSchemaVersion {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin configuration schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if version == 0 {
		if _, err := tx.ExecContext(ctx, initialSchema); err != nil {
			return fmt.Errorf("create initial configuration schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("set configuration schema version: %w", err)
		}
	} else {
		return fmt.Errorf("no migration path from configuration schema version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit configuration schema migration: %w", err)
	}
	return nil
}

const initialSchema = `
CREATE TABLE providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    provider_type TEXT NOT NULL,
    base_url TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'disconnected',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE provider_registrations (
    provider_id TEXT PRIMARY KEY REFERENCES providers(id) ON DELETE CASCADE,
    issued_client_id TEXT NOT NULL,
    verified_subject TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
CREATE TABLE provider_credentials (
    provider_id TEXT PRIMARY KEY REFERENCES provider_registrations(provider_id) ON DELETE CASCADE,
    access_token TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    id_token TEXT NOT NULL,
    expires_at TEXT,
    earliest_refresh_at TEXT,
    scopes_json TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE service_accounts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE local_keys (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES service_accounts(id) ON DELETE CASCADE,
    key_hash BLOB NOT NULL UNIQUE,
    display_hint TEXT NOT NULL,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE INDEX local_keys_by_account ON local_keys(account_id, created_at);
CREATE TABLE models (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    strategy TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version >= 1),
    strategy_config_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(strategy_config_json)),
    provider_id TEXT REFERENCES providers(id) ON DELETE RESTRICT,
    upstream_model TEXT,
    metadata_json TEXT CHECK (metadata_json IS NULL OR json_valid(metadata_json)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE model_grants (
    account_id TEXT NOT NULL REFERENCES service_accounts(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (account_id, model_id)
);
CREATE INDEX model_grants_by_model ON model_grants(model_id, account_id);
CREATE TABLE settings_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

func nowText() (time.Time, string) {
	now := time.Now().UTC()
	return now, now.Format("2006-01-02T15:04:05.000000000Z")
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func nullableRawJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func readTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp: %w", err)
	}
	return parsed.UTC(), nil
}

func readNullableTime(value sql.NullString) (time.Time, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, nil
	}
	return readTime(value.String)
}

func requireAffected(result sql.Result, kind, id string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check %s update: %w", kind, err)
	}
	if affected == 0 {
		return notFound(kind, id)
	}
	return nil
}

func notFound(kind, id string) error {
	return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id)
}
