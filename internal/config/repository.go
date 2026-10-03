package config

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type scanner interface {
	Scan(dest ...any) error
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate UUID: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

// GetOrCreateHostID returns the persistent host UUID as a URN. Concurrent callers
// use one transactionally inserted value; a process restart reads the same value.
func (s *Store) GetOrCreateHostID(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin host ID lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var hostID string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings_metadata WHERE key = 'host_id'`).Scan(&hostID)
	if errors.Is(err, sql.ErrNoRows) {
		id, idErr := newUUID()
		if idErr != nil {
			return "", idErr
		}
		candidate := "urn:uuid:" + id
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings_metadata(key, value) VALUES ('host_id', ?) ON CONFLICT(key) DO NOTHING`, candidate); err != nil {
			return "", fmt.Errorf("persist host ID: %w", err)
		}
		err = tx.QueryRowContext(ctx, `SELECT value FROM settings_metadata WHERE key = 'host_id'`).Scan(&hostID)
	}
	if err != nil {
		return "", fmt.Errorf("read host ID: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit host ID lookup: %w", err)
	}
	return hostID, nil
}

// CreateProvider inserts a Provider and fills its stable ID and timestamps.
func (s *Store) CreateProvider(ctx context.Context, provider Provider) (Provider, error) {
	if strings.TrimSpace(provider.Name) == "" || strings.TrimSpace(provider.Type) == "" || strings.TrimSpace(provider.BaseURL) == "" {
		return Provider{}, errors.New("provider name, type, and base URL are required")
	}
	if provider.ID == "" {
		id, err := newUUID()
		if err != nil {
			return Provider{}, err
		}
		provider.ID = id
	}
	if provider.Status == "" {
		provider.Status = "disconnected"
	}
	now, timestamp := nowText()
	provider.CreatedAt, provider.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO providers(id, name, provider_type, base_url, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		provider.ID, provider.Name, provider.Type, provider.BaseURL, provider.Status, timestamp, timestamp)
	if err != nil {
		if isUniqueConstraint(err) {
			return Provider{}, ErrConflict
		}
		return Provider{}, fmt.Errorf("create provider: %w", err)
	}
	return provider, nil
}

// UpdateProvider changes the UI-safe fields of an existing Provider.
func (s *Store) UpdateProvider(ctx context.Context, provider Provider) error {
	if strings.TrimSpace(provider.Name) == "" || strings.TrimSpace(provider.Type) == "" || strings.TrimSpace(provider.BaseURL) == "" {
		return errors.New("provider name, type, and base URL are required")
	}
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE providers SET name = ?, provider_type = ?, base_url = ?, status = ?, updated_at = ? WHERE id = ?`,
		provider.Name, provider.Type, provider.BaseURL, provider.Status, timestamp, provider.ID)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrConflict
		}
		return fmt.Errorf("update provider: %w", err)
	}
	return requireAffected(result, "provider", provider.ID)
}

// RenameProvider changes only the display name so concurrent OAuth status updates survive.
func (s *Store) RenameProvider(ctx context.Context, id, name string) error {
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE providers SET name = ?, updated_at = ? WHERE id = ?`, name, timestamp, id)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrConflict
		}
		return fmt.Errorf("rename provider: %w", err)
	}
	return requireAffected(result, "provider", id)
}

// DeleteProvider removes a Provider and its credentials. Models targeting it prevent deletion.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		if isForeignKeyConstraint(err) || isRestrictConstraint(err) {
			return ErrReferenced
		}
		return fmt.Errorf("delete provider: %w", err)
	}
	return requireAffected(result, "provider", id)
}

// GetProvider returns one Provider without credentials.
func (s *Store) GetProvider(ctx context.Context, id string) (Provider, error) {
	provider, err := scanProvider(s.db.QueryRowContext(ctx, `SELECT id, name, provider_type, base_url, status, created_at, updated_at FROM providers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, notFound("provider", id)
	}
	if err != nil {
		return Provider{}, fmt.Errorf("get provider: %w", err)
	}
	return provider, nil
}

// ListProviders returns Providers in stable creation order without credentials.
func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, provider_type, base_url, status, created_at, updated_at FROM providers ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	providers := make([]Provider, 0)
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			return nil, fmt.Errorf("read provider: %w", err)
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	return providers, nil
}

// SaveProviderRegistration stores an issued client ID before the first token exchange.
// Existing verified identity fields are preserved when a registration is reused.
func (s *Store) SaveProviderRegistration(ctx context.Context, registration ProviderRegistration) error {
	if registration.ProviderID == "" || strings.TrimSpace(registration.IssuedClientID) == "" {
		return errors.New("provider ID and issued OAuth client ID are required")
	}
	_, timestamp := nowText()
	_, err := s.db.ExecContext(ctx, `INSERT INTO provider_registrations(provider_id, issued_client_id, verified_subject, email, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET issued_client_id = excluded.issued_client_id, updated_at = excluded.updated_at`,
		registration.ProviderID, registration.IssuedClientID, registration.VerifiedSubject, registration.Email, timestamp)
	if err != nil {
		return fmt.Errorf("save provider registration: %w", err)
	}
	return nil
}

// ReplaceProviderCredentials atomically replaces a Provider's full token set and
// registration identity. Tokens are never written through Provider or returned by list methods.
func (s *Store) ReplaceProviderCredentials(ctx context.Context, credentials ProviderCredentials) error {
	return s.ReplaceProviderCredentialsWithStatus(ctx, credentials, "connected")
}

// ReplaceProviderCredentialsWithStatus atomically saves one complete OAuth token set,
// its verified registration identity, and the state exposed to the management UI.
func (s *Store) ReplaceProviderCredentialsWithStatus(ctx context.Context, credentials ProviderCredentials, status string) error {
	if status != "connected" && status != "plan_usage_disabled" && status != "require_reauthentication" {
		return errors.New("invalid provider OAuth status")
	}
	if credentials.ProviderID == "" || credentials.IssuedClientID == "" {
		return errors.New("provider ID and issued OAuth client ID are required")
	}
	if len(credentials.EarliestRefreshAt) != 0 && !json.Valid(credentials.EarliestRefreshAt) {
		return errors.New("provider refresh metadata must be valid JSON")
	}
	scopes := credentials.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return fmt.Errorf("encode provider OAuth scopes: %w", err)
	}
	_, timestamp := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin credential replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_registrations(provider_id, issued_client_id, verified_subject, email, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET issued_client_id = excluded.issued_client_id,
		verified_subject = excluded.verified_subject, email = excluded.email, updated_at = excluded.updated_at`,
		credentials.ProviderID, credentials.IssuedClientID, credentials.VerifiedSubject, credentials.Email, timestamp)
	if err != nil {
		return fmt.Errorf("save provider registration: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO provider_credentials(provider_id, access_token, refresh_token, id_token, expires_at, earliest_refresh_at, scopes_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET access_token = excluded.access_token,
		refresh_token = excluded.refresh_token, id_token = excluded.id_token, expires_at = excluded.expires_at,
		earliest_refresh_at = excluded.earliest_refresh_at, scopes_json = excluded.scopes_json, updated_at = excluded.updated_at`,
		credentials.ProviderID, credentials.AccessToken, credentials.RefreshToken, credentials.IDToken,
		nullableTime(credentials.ExpiresAt), nullableRawJSON(credentials.EarliestRefreshAt), string(scopesJSON), timestamp)
	if err != nil {
		return fmt.Errorf("save provider credential set: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE providers SET status = ?, updated_at = ? WHERE id = ?`, status, timestamp, credentials.ProviderID)
	if err != nil {
		return fmt.Errorf("update provider connection status: %w", err)
	}
	if err := requireAffected(result, "provider", credentials.ProviderID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider credential replacement: %w", err)
	}
	return nil
}

// GetProviderRegistration returns the persistent OAuth registration identity.
func (s *Store) GetProviderRegistration(ctx context.Context, providerID string) (ProviderRegistration, error) {
	var registration ProviderRegistration
	registration.ProviderID = providerID
	err := s.db.QueryRowContext(ctx, `SELECT issued_client_id, verified_subject, email FROM provider_registrations WHERE provider_id = ?`, providerID).Scan(
		&registration.IssuedClientID, &registration.VerifiedSubject, &registration.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderRegistration{}, notFound("provider registration", providerID)
	}
	if err != nil {
		return ProviderRegistration{}, fmt.Errorf("get provider registration: %w", err)
	}
	return registration, nil
}

// GetProviderCredentials returns registration identity and the complete current
// token set. After disconnect, the identity remains and token fields are empty.
func (s *Store) GetProviderCredentials(ctx context.Context, providerID string) (ProviderCredentials, error) {
	var credentials ProviderCredentials
	var expiresAt, earliestRefreshAt sql.NullString
	var scopesJSON string
	err := s.db.QueryRowContext(ctx, `SELECT r.issued_client_id, r.verified_subject, r.email,
		COALESCE(c.access_token, ''), COALESCE(c.refresh_token, ''), COALESCE(c.id_token, ''),
		c.expires_at, c.earliest_refresh_at, COALESCE(c.scopes_json, '[]')
		FROM provider_registrations r LEFT JOIN provider_credentials c ON c.provider_id = r.provider_id
		WHERE r.provider_id = ?`, providerID).Scan(
		&credentials.IssuedClientID, &credentials.VerifiedSubject, &credentials.Email,
		&credentials.AccessToken, &credentials.RefreshToken, &credentials.IDToken,
		&expiresAt, &earliestRefreshAt, &scopesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderCredentials{}, notFound("provider registration", providerID)
	}
	if err != nil {
		return ProviderCredentials{}, fmt.Errorf("get provider credentials: %w", err)
	}
	credentials.ProviderID = providerID
	if credentials.ExpiresAt, err = readNullableTime(expiresAt); err != nil {
		return ProviderCredentials{}, fmt.Errorf("read provider credential expiry: %w", err)
	}
	if earliestRefreshAt.Valid {
		credentials.EarliestRefreshAt = json.RawMessage(earliestRefreshAt.String)
	}
	if err := json.Unmarshal([]byte(scopesJSON), &credentials.Scopes); err != nil {
		return ProviderCredentials{}, fmt.Errorf("decode provider OAuth scopes: %w", err)
	}
	return credentials, nil
}

// DisconnectProvider clears all OAuth tokens but preserves the registration identity.
func (s *Store) DisconnectProvider(ctx context.Context, providerID string) error {
	_, timestamp := nowText()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin provider disconnection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM provider_credentials WHERE provider_id = ?`, providerID); err != nil {
		return fmt.Errorf("clear provider credentials: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE providers SET status = 'disconnected', updated_at = ? WHERE id = ?`, timestamp, providerID)
	if err != nil {
		return fmt.Errorf("update provider disconnection status: %w", err)
	}
	if err := requireAffected(result, "provider", providerID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit provider disconnection: %w", err)
	}
	return nil
}

func scanProvider(row scanner) (Provider, error) {
	var provider Provider
	var createdAt, updatedAt string
	err := row.Scan(&provider.ID, &provider.Name, &provider.Type, &provider.BaseURL, &provider.Status, &createdAt, &updatedAt)
	if err != nil {
		return Provider{}, err
	}
	if provider.CreatedAt, err = readTime(createdAt); err != nil {
		return Provider{}, err
	}
	if provider.UpdatedAt, err = readTime(updatedAt); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

// CreateServiceAccount inserts an account and fills its stable ID and timestamps.
func (s *Store) CreateServiceAccount(ctx context.Context, account ServiceAccount) (ServiceAccount, error) {
	if strings.TrimSpace(account.Name) == "" {
		return ServiceAccount{}, errors.New("service account name is required")
	}
	if account.ID == "" {
		id, err := newUUID()
		if err != nil {
			return ServiceAccount{}, err
		}
		account.ID = id
	}
	now, timestamp := nowText()
	account.CreatedAt, account.UpdatedAt = now, now
	_, err := s.db.ExecContext(ctx, `INSERT INTO service_accounts(id, name, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		account.ID, account.Name, boolInt(account.Enabled), timestamp, timestamp)
	if err != nil {
		if isUniqueConstraint(err) {
			return ServiceAccount{}, ErrConflict
		}
		return ServiceAccount{}, fmt.Errorf("create service account: %w", err)
	}
	return account, nil
}

// UpdateServiceAccount changes an account's name and enabled state.
func (s *Store) UpdateServiceAccount(ctx context.Context, account ServiceAccount) error {
	if strings.TrimSpace(account.Name) == "" {
		return errors.New("service account name is required")
	}
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE service_accounts SET name = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		account.Name, boolInt(account.Enabled), timestamp, account.ID)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrConflict
		}
		return fmt.Errorf("update service account: %w", err)
	}
	return requireAffected(result, "service account", account.ID)
}

// DeleteServiceAccount removes an account, its local keys, and its explicit grants.
func (s *Store) DeleteServiceAccount(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM service_accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete service account: %w", err)
	}
	return requireAffected(result, "service account", id)
}

// GetServiceAccount returns one local account.
func (s *Store) GetServiceAccount(ctx context.Context, id string) (ServiceAccount, error) {
	account, err := scanServiceAccount(s.db.QueryRowContext(ctx, `SELECT id, name, enabled, created_at, updated_at FROM service_accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceAccount{}, notFound("service account", id)
	}
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("get service account: %w", err)
	}
	return account, nil
}

// ListServiceAccounts returns local accounts in stable creation order.
func (s *Store) ListServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, enabled, created_at, updated_at FROM service_accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list service accounts: %w", err)
	}
	defer rows.Close()
	accounts := make([]ServiceAccount, 0)
	for rows.Next() {
		account, err := scanServiceAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("read service account: %w", err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list service accounts: %w", err)
	}
	return accounts, nil
}

// CreateLocalKey stores a supplied cryptographic hash and display hint, never a plaintext key.
func (s *Store) CreateLocalKey(ctx context.Context, accountID string, hash []byte, displayHint string) (LocalKey, error) {
	if len(hash) == 0 {
		return LocalKey{}, errors.New("local key hash is required")
	}
	if displayHint == "" {
		return LocalKey{}, errors.New("local key display hint is required")
	}
	id, err := newUUID()
	if err != nil {
		return LocalKey{}, err
	}
	now, timestamp := nowText()
	key := LocalKey{ID: id, AccountID: accountID, DisplayHint: displayHint, CreatedAt: now}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_keys(id, account_id, key_hash, display_hint, created_at) VALUES (?, ?, ?, ?, ?)`,
		key.ID, key.AccountID, append([]byte(nil), hash...), key.DisplayHint, timestamp)
	if err != nil {
		if isUniqueConstraint(err) {
			return LocalKey{}, ErrConflict
		}
		if isForeignKeyConstraint(err) {
			return LocalKey{}, notFound("service account", accountID)
		}
		return LocalKey{}, fmt.Errorf("create local key: %w", err)
	}
	return key, nil
}

// ListLocalKeys returns key metadata for one account without returning any key hashes.
func (s *Store) ListLocalKeys(ctx context.Context, accountID string) ([]LocalKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account_id, display_hint, created_at, revoked_at FROM local_keys WHERE account_id = ? ORDER BY created_at, id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list local keys: %w", err)
	}
	defer rows.Close()
	keys := make([]LocalKey, 0)
	for rows.Next() {
		key, err := scanLocalKey(rows)
		if err != nil {
			return nil, fmt.Errorf("read local key metadata: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list local keys: %w", err)
	}
	return keys, nil
}

// LookupLocalKey finds an unrevoked key by hash and returns its account. It never
// returns or logs a plaintext key; a hash is used only as a SQL lookup argument.
func (s *Store) LookupLocalKey(ctx context.Context, hash []byte) (LocalKey, ServiceAccount, error) {
	var key LocalKey
	var account ServiceAccount
	var keyCreatedAt, accountCreatedAt, accountUpdatedAt string
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT k.id, k.account_id, k.display_hint, k.created_at,
		a.id, a.name, a.enabled, a.created_at, a.updated_at
		FROM local_keys k JOIN service_accounts a ON a.id = k.account_id
		WHERE k.key_hash = ? AND k.revoked_at IS NULL AND a.enabled = 1`, hash).Scan(
		&key.ID, &key.AccountID, &key.DisplayHint, &keyCreatedAt,
		&account.ID, &account.Name, &enabled, &accountCreatedAt, &accountUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalKey{}, ServiceAccount{}, ErrNotFound
	}
	if err != nil {
		return LocalKey{}, ServiceAccount{}, fmt.Errorf("lookup local key: %w", err)
	}
	key.CreatedAt, err = readTime(keyCreatedAt)
	if err != nil {
		return LocalKey{}, ServiceAccount{}, fmt.Errorf("read local key creation time: %w", err)
	}
	account.Enabled = enabled != 0
	if account.CreatedAt, err = readTime(accountCreatedAt); err != nil {
		return LocalKey{}, ServiceAccount{}, fmt.Errorf("read service account creation time: %w", err)
	}
	if account.UpdatedAt, err = readTime(accountUpdatedAt); err != nil {
		return LocalKey{}, ServiceAccount{}, fmt.Errorf("read service account update time: %w", err)
	}
	return key, account, nil
}

// RevokeLocalKey revokes a local key. Repeating the operation leaves its original
// revocation timestamp intact.
func (s *Store) RevokeLocalKey(ctx context.Context, keyID string) error {
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE local_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, timestamp, keyID)
	if err != nil {
		return fmt.Errorf("revoke local key: %w", err)
	}
	return requireAffected(result, "local key", keyID)
}

func scanServiceAccount(row scanner) (ServiceAccount, error) {
	var account ServiceAccount
	var enabled int
	var createdAt, updatedAt string
	err := row.Scan(&account.ID, &account.Name, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return ServiceAccount{}, err
	}
	account.Enabled = enabled != 0
	if account.CreatedAt, err = readTime(createdAt); err != nil {
		return ServiceAccount{}, err
	}
	if account.UpdatedAt, err = readTime(updatedAt); err != nil {
		return ServiceAccount{}, err
	}
	return account, nil
}

func scanLocalKey(row scanner) (LocalKey, error) {
	var key LocalKey
	var createdAt string
	var revokedAt sql.NullString
	if err := row.Scan(&key.ID, &key.AccountID, &key.DisplayHint, &createdAt, &revokedAt); err != nil {
		return LocalKey{}, err
	}
	var err error
	if key.CreatedAt, err = readTime(createdAt); err != nil {
		return LocalKey{}, err
	}
	if revokedAt.Valid {
		t, err := readTime(revokedAt.String)
		if err != nil {
			return LocalKey{}, err
		}
		key.RevokedAt = &t
	}
	return key, nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// CreateModel inserts a routing configuration at revision 1 and fills its stable ID and timestamps.
func (s *Store) CreateModel(ctx context.Context, model Model) (Model, error) {
	if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Strategy) == "" {
		return Model{}, errors.New("model name and strategy are required")
	}
	if model.ID == "" {
		id, err := newUUID()
		if err != nil {
			return Model{}, err
		}
		model.ID = id
	}
	model.Version = 1
	strategyConfig, metadata, err := modelJSON(model)
	if err != nil {
		return Model{}, err
	}
	now, timestamp := nowText()
	model.CreatedAt, model.UpdatedAt = now, now
	_, err = s.db.ExecContext(ctx, `INSERT INTO models(id, name, strategy, version, strategy_config_json, provider_id, upstream_model, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, model.ID, model.Name, model.Strategy, model.Version,
		string(strategyConfig), nullableString(model.ProviderID), nullableString(model.UpstreamModel), nullableJSON(metadata), timestamp, timestamp)
	if err != nil {
		if isUniqueConstraint(err) {
			return Model{}, ErrConflict
		}
		if isForeignKeyConstraint(err) {
			return Model{}, ErrNotFound
		}
		return Model{}, fmt.Errorf("create model: %w", err)
	}
	model.StrategyConfigJSON = strategyConfig
	model.MetadataJSON = metadata
	return model, nil
}

// UpdateModel changes a routing configuration and increments its revision atomically.
func (s *Store) UpdateModel(ctx context.Context, model Model) error {
	if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Strategy) == "" {
		return errors.New("model name and strategy are required")
	}
	strategyConfig, metadata, err := modelJSON(model)
	if err != nil {
		return err
	}
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE models SET name = ?, strategy = ?, version = version + 1, strategy_config_json = ?, provider_id = ?, upstream_model = ?, metadata_json = ?, updated_at = ? WHERE id = ?`,
		model.Name, model.Strategy, string(strategyConfig), nullableString(model.ProviderID), nullableString(model.UpstreamModel), nullableJSON(metadata), timestamp, model.ID)
	if err != nil {
		if isUniqueConstraint(err) {
			return ErrConflict
		}
		if isForeignKeyConstraint(err) {
			return ErrNotFound
		}
		return fmt.Errorf("update model: %w", err)
	}
	return requireAffected(result, "model", model.ID)
}

// DeleteModel removes the model and its explicit grants.
func (s *Store) DeleteModel(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	return requireAffected(result, "model", id)
}

// GetModel returns one model configuration.
func (s *Store) GetModel(ctx context.Context, id string) (Model, error) {
	model, err := scanModel(s.db.QueryRowContext(ctx, `SELECT id, name, strategy, version, strategy_config_json, provider_id, upstream_model, metadata_json, created_at, updated_at FROM models WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, notFound("model", id)
	}
	if err != nil {
		return Model{}, fmt.Errorf("get model: %w", err)
	}
	return model, nil
}

// GetModelByName returns one model configuration by its unique local name.
func (s *Store) GetModelByName(ctx context.Context, name string) (Model, error) {
	model, err := scanModel(s.db.QueryRowContext(ctx, `SELECT id, name, strategy, version, strategy_config_json, provider_id, upstream_model, metadata_json, created_at, updated_at FROM models WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, notFound("model", name)
	}
	if err != nil {
		return Model{}, fmt.Errorf("get model by name: %w", err)
	}
	return model, nil
}

// ListModels returns all model configurations in stable creation order.
func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, strategy, version, strategy_config_json, provider_id, upstream_model, metadata_json, created_at, updated_at FROM models ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	models := make([]Model, 0)
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("read model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	return models, nil
}

// ReplaceModelGrants atomically replaces an account's explicit model permissions.
// The empty list revokes all grants; foreign keys reject unknown account or models.
func (s *Store) ReplaceModelGrants(ctx context.Context, accountID string, modelIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin model grant replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingAccount string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM service_accounts WHERE id = ?`, accountID).Scan(&existingAccount); errors.Is(err, sql.ErrNoRows) {
		return notFound("service account", accountID)
	} else if err != nil {
		return fmt.Errorf("check service account before grant replacement: %w", err)
	}
	seen := make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, duplicate := seen[modelID]; duplicate {
			continue
		}
		seen[modelID] = struct{}{}
		var existingModel string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM models WHERE id = ?`, modelID).Scan(&existingModel); errors.Is(err, sql.ErrNoRows) {
			return notFound("model", modelID)
		} else if err != nil {
			return fmt.Errorf("check model before grant replacement: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_grants WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("clear existing model grants: %w", err)
	}
	seen = make(map[string]struct{}, len(modelIDs))
	for _, modelID := range modelIDs {
		if _, duplicate := seen[modelID]; duplicate {
			continue
		}
		seen[modelID] = struct{}{}
		_, timestamp := nowText()
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_grants(account_id, model_id, created_at) VALUES (?, ?, ?)`, accountID, modelID, timestamp); err != nil {
			return fmt.Errorf("replace model grants: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit model grant replacement: %w", err)
	}
	return nil
}

// ListModelGrants returns grant identities for one account.
func (s *Store) ListModelGrants(ctx context.Context, accountID string) ([]ModelGrant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, model_id, created_at FROM model_grants WHERE account_id = ? ORDER BY created_at, model_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list model grants: %w", err)
	}
	defer rows.Close()
	grants := make([]ModelGrant, 0)
	for rows.Next() {
		var grant ModelGrant
		var createdAt string
		if err := rows.Scan(&grant.AccountID, &grant.ModelID, &createdAt); err != nil {
			return nil, fmt.Errorf("read model grant: %w", err)
		}
		grant.CreatedAt, err = readTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("read model grant time: %w", err)
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model grants: %w", err)
	}
	return grants, nil
}

// ListGrantedModels returns the configured models explicitly granted to an account.
func (s *Store) ListGrantedModels(ctx context.Context, accountID string) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.name, m.strategy, m.version, m.strategy_config_json, m.provider_id, m.upstream_model, m.metadata_json, m.created_at, m.updated_at
		FROM models m JOIN model_grants g ON g.model_id = m.id WHERE g.account_id = ? ORDER BY m.created_at, m.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list granted models: %w", err)
	}
	defer rows.Close()
	models := make([]Model, 0)
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("read granted model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list granted models: %w", err)
	}
	return models, nil
}

// HasModelGrant checks one explicit account-to-model permission.
func (s *Store) HasModelGrant(ctx context.Context, accountID, modelID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM model_grants WHERE account_id = ? AND model_id = ?`, accountID, modelID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check model grant: %w", err)
	}
	return true, nil
}

func modelJSON(model Model) (json.RawMessage, json.RawMessage, error) {
	strategyConfig := model.StrategyConfigJSON
	if len(strategyConfig) == 0 {
		strategyConfig = json.RawMessage(`{}`)
	}
	if !json.Valid(strategyConfig) {
		return nil, nil, errors.New("model strategy configuration must be valid JSON")
	}
	metadata := model.MetadataJSON
	if len(metadata) != 0 && !json.Valid(metadata) {
		return nil, nil, errors.New("model metadata must be valid JSON")
	}
	return append(json.RawMessage(nil), strategyConfig...), append(json.RawMessage(nil), metadata...), nil
}

func scanModel(row scanner) (Model, error) {
	var model Model
	var strategyConfig []byte
	var providerID, upstreamModel, metadata sql.NullString
	var createdAt, updatedAt string
	err := row.Scan(&model.ID, &model.Name, &model.Strategy, &model.Version, &strategyConfig, &providerID, &upstreamModel, &metadata, &createdAt, &updatedAt)
	if err != nil {
		return Model{}, err
	}
	model.StrategyConfigJSON = append(json.RawMessage(nil), strategyConfig...)
	if providerID.Valid {
		model.ProviderID = providerID.String
	}
	if upstreamModel.Valid {
		model.UpstreamModel = upstreamModel.String
	}
	if metadata.Valid {
		model.MetadataJSON = json.RawMessage(metadata.String)
	}
	if model.CreatedAt, err = readTime(createdAt); err != nil {
		return Model{}, err
	}
	if model.UpdatedAt, err = readTime(updatedAt); err != nil {
		return Model{}, err
	}
	return model, nil
}

// RevokeLocalKeyForAccount revokes a key only when it belongs to the supplied account.
func (s *Store) RevokeLocalKeyForAccount(ctx context.Context, accountID, keyID string) error {
	_, timestamp := nowText()
	result, err := s.db.ExecContext(ctx, `UPDATE local_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND account_id = ?`, timestamp, keyID, accountID)
	if err != nil {
		return fmt.Errorf("revoke service account key: %w", err)
	}
	return requireAffected(result, "local key", keyID)
}

// ResolveGrantedRoute reads the current key/account state, explicit grant, Model,
// and Provider in one read transaction. The returned values form an admission-time
// snapshot for an inference request; callers should release it before upstream I/O.
func (s *Store) ResolveGrantedRoute(ctx context.Context, accountID, keyID, modelName string) (ServiceAccount, Model, Provider, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ServiceAccount{}, Model{}, Provider{}, fmt.Errorf("begin route snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	account, err := activeAccountForKey(ctx, tx, accountID, keyID)
	if err != nil {
		return ServiceAccount{}, Model{}, Provider{}, err
	}
	model, err := scanModel(tx.QueryRowContext(ctx, `SELECT id, name, strategy, version, strategy_config_json, provider_id, upstream_model, metadata_json, created_at, updated_at FROM models WHERE name = ?`, modelName))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceAccount{}, Model{}, Provider{}, notFound("model", modelName)
	}
	if err != nil {
		return ServiceAccount{}, Model{}, Provider{}, fmt.Errorf("read model for route: %w", err)
	}
	var grant string
	err = tx.QueryRowContext(ctx, `SELECT model_id FROM model_grants WHERE account_id = ? AND model_id = ?`, accountID, model.ID).Scan(&grant)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceAccount{}, Model{}, Provider{}, ErrForbidden
	}
	if err != nil {
		return ServiceAccount{}, Model{}, Provider{}, fmt.Errorf("read route grant: %w", err)
	}
	if model.ProviderID == "" {
		return ServiceAccount{}, Model{}, Provider{}, errors.New("configured model has no provider target")
	}
	provider, err := scanProvider(tx.QueryRowContext(ctx, `SELECT id, name, provider_type, base_url, status, created_at, updated_at FROM providers WHERE id = ?`, model.ProviderID))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceAccount{}, Model{}, Provider{}, errors.New("configured model provider is missing")
	}
	if err != nil {
		return ServiceAccount{}, Model{}, Provider{}, fmt.Errorf("read route provider: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServiceAccount{}, Model{}, Provider{}, fmt.Errorf("commit route snapshot: %w", err)
	}
	return account, model, provider, nil
}

// ListGrantedModelsForKey validates the active key in the same transaction used
// to snapshot its current explicit Model grants.
func (s *Store) ListGrantedModelsForKey(ctx context.Context, accountID, keyID string) ([]Model, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin authorized model listing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := activeAccountForKey(ctx, tx, accountID, keyID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.name, m.strategy, m.version, m.strategy_config_json, m.provider_id, m.upstream_model, m.metadata_json, m.created_at, m.updated_at
		FROM models m JOIN model_grants g ON g.model_id = m.id WHERE g.account_id = ? ORDER BY m.created_at, m.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("read authorized models: %w", err)
	}
	models := make([]Model, 0)
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read authorized model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read authorized models: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close authorized model listing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit authorized model listing: %w", err)
	}
	return models, nil
}

func activeAccountForKey(ctx context.Context, tx *sql.Tx, accountID, keyID string) (ServiceAccount, error) {
	var account ServiceAccount
	var enabled int
	var createdAt, updatedAt string
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.name, a.enabled, a.created_at, a.updated_at
		FROM service_accounts a JOIN local_keys k ON k.account_id = a.id
		WHERE a.id = ? AND k.id = ? AND k.revoked_at IS NULL AND a.enabled = 1`, accountID, keyID).Scan(
		&account.ID, &account.Name, &enabled, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceAccount{}, ErrUnauthorized
	}
	if err != nil {
		return ServiceAccount{}, fmt.Errorf("validate service account key: %w", err)
	}
	account.Enabled = enabled != 0
	if account.CreatedAt, err = readTime(createdAt); err != nil {
		return ServiceAccount{}, fmt.Errorf("read service account creation time: %w", err)
	}
	if account.UpdatedAt, err = readTime(updatedAt); err != nil {
		return ServiceAccount{}, fmt.Errorf("read service account update time: %w", err)
	}
	return account, nil
}

func isUniqueConstraint(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	return coded.Code() == 1555 || coded.Code() == 2067
}

func isForeignKeyConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code() == 787
}

// SQLite implements ON DELETE RESTRICT with a constraint trigger.
func isRestrictConstraint(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code() == 1811
}
