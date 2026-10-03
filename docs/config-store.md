# SQLite configuration store

Gyemoim creates `${data-directory}/config.db` automatically with owner-only file permissions. The process opens it only after acquiring the data-directory lock, and closes it before releasing that lock. The application uses `modernc.org/sqlite` v1.59.0 with CGO disabled, one pooled connection, foreign-key enforcement, WAL journaling, and a five-second busy timeout.

Schema migrations run in a transaction and use SQLite's `user_version`. A database with a version newer than the running executable supports causes startup to stop with a clear error; migration code does not reset or discard user data.

The database contains configuration only:

- `providers`: non-secret connection names, type, base URL, status, and timestamps.
- `provider_registrations`: issued OAuth client ID and verified account identity, stored separately from tokens.
- `provider_credentials`: one replaceable set of OAuth tokens, expiry times, and scopes per Provider. Token replacement is atomic. Disconnect removes this row and keeps the registration identity.
- `service_accounts` and `local_keys`: local account metadata and key hashes. Plaintext keys are never stored.
- `models`: unique local names, strategy and revision, strategy configuration JSON, an optional provider foreign key and upstream model ID, and optional metadata JSON. The target lives in typed columns rather than being copied into strategy JSON.
- `model_grants`: explicit ServiceAccount-to-Model permissions with foreign keys.
- `settings_metadata`: stable host ID and future application settings.

Request bodies, provider usage, and request history do not belong in this database; they are stored in NDJSON files by later implementation tasks.

## Read-only manual inspection

With the service stopped, use SQLite's read-only mode to inspect the database. Replace the path with the active data directory:

```sh
sqlite3 -readonly "$HOME/.local/share/gyemoim/config.db"
```

For an XDG data directory, use `$XDG_DATA_HOME/gyemoim/config.db`; on macOS, use `"$HOME/Library/Application Support/Gyemoim/config.db"`. Then run read-only queries such as:

```sql
PRAGMA user_version;
PRAGMA journal_mode;
SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name;
SELECT key, value FROM settings_metadata WHERE key = 'host_id';
SELECT id, name, provider_type, status FROM providers ORDER BY name;
SELECT p.id, p.name, CASE WHEN c.provider_id IS NULL THEN 'no token set' ELSE 'tokens stored' END AS credential_state
FROM providers AS p LEFT JOIN provider_credentials AS c ON c.provider_id = p.id ORDER BY p.name;
SELECT id, name, strategy, version, provider_id, upstream_model FROM models ORDER BY name;
SELECT account_id, model_id, created_at FROM model_grants ORDER BY account_id, model_id;
SELECT id, account_id, display_hint, created_at, revoked_at FROM local_keys ORDER BY account_id, created_at;
```

Do not select or export rows from `provider_credentials` or the `key_hash` column in `local_keys`; those columns contain authentication material. `foreign_keys` and `busy_timeout` are connection settings, so a separate SQLite shell does not report the application's connection values. WAL mode persists in the database and is visible to the shell.
