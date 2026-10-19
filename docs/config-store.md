# SQLite configuration store

Gyemoim creates `${data-directory}/config.db` automatically with owner-only file permissions. The process opens it only after acquiring the data-directory lock, and closes it before releasing that lock. The application uses `modernc.org/sqlite` v1.59.0 with CGO disabled, one pooled connection, foreign-key enforcement, WAL journaling, and a five-second busy timeout.

Schema migrations run in a transaction and use SQLite's `user_version` (currently 5). A database with a version newer than the running executable supports causes startup to stop with a clear error; migration code does not reset or discard user data.

The database contains configuration only:

- `providers`: non-secret connection names, type, base URL, status, and timestamps.
- `provider_registrations`: issued OAuth client ID and verified account identity, stored separately from tokens.
- `provider_credentials`: one replaceable set of OAuth tokens, expiry times, and scopes per Provider. Token replacement is atomic. Disconnect removes this row and keeps the registration identity.
- `service_accounts` and `local_keys`: local account metadata and key hashes. Plaintext keys are never stored.
- `models`: unique local names, strategy and revision, strategy configuration JSON, and optional metadata JSON. Legacy `provider_id` and `upstream_model` columns mirror the first target; target-table triggers maintain this projection.
- `model_targets`: canonical ordered destinations in typed columns, not strategy JSON. Each row contains `model_id`, zero-based `position`, `provider_id`, and `upstream_model`. The primary key is `(model_id, position)`; `(model_id, provider_id, upstream_model)` is unique. Provider foreign keys restrict deletion of every referenced provider, not just the first; deleting an ungranted Model cascades its targets.
- `model_grants`: explicit ServiceAccount-to-Model permissions with foreign keys.
- `settings_metadata`: stable host ID and future application settings.

Request bodies, provider usage, and request history do not belong in this database; they are stored in NDJSON files.

## Model target contract

The v4-to-v5 migration copies each existing target to position 0 atomically, preserving model IDs, revisions, timestamps, and grants. Legacy models with no target remain readable with an empty target list. A partial or invalid legacy target stops and rolls back migration rather than silently dropping configuration.

The Go configuration store exposes `Model.Targets` as an ordered list of `ModelTarget{ProviderID, UpstreamModel}`. A non-nil list is authoritative on create/update; a nil list accepts the legacy `ProviderID`/`UpstreamModel` pair. Read values and create results always project the first target into those legacy fields. To change a model returned by the store, modify `Targets`, not just the projected pair. The list and JSON configuration are copied rather than retained from the caller.

Create and full replacement (including reorder) are transactional. New or updated configurations require at least one complete target, existing providers, and no duplicate provider/upstream-model pairs. The same provider can occur with different upstream models. The `single` strategy requires exactly one target. Creation starts at revision 1; each successful replacement increments the revision once, including when target order changes. Failed writes leave the model, revision, and target list unchanged. Reordering uses full replacement, with contiguous positions assigned from list order.

All model reads include the ordered targets in the same SQLite statement as the model fields. Authorized route reads additionally snapshot the active key/account, grant, and all referenced Providers in one transaction, released before upstream I/O. `ResolveGrantedTargets` returns the provider map; `ResolveGrantedRoute` preserves the legacy first-Provider projection of the same full snapshot. The canonical target table is the read source; legacy model columns are compatibility projections, never an independent routing source. Direct SQL configuration writes are unsupported.

Management `POST /api/models` and `PUT /api/models/{id}` accept `name`, `strategy`, ordered `targets: [{providerId, upstreamModel}]`, and `metadata`. Omitted strategy defaults to `single` for legacy clients; omitted targets accept the legacy `providerId`/`upstreamModel` pair. A supplied target list is authoritative. Responses expose ordered `targets` and retain the first-target projections. Only `single` and explicitly opted-in `fallback` are accepted; single requires exactly one target, fallback 1–128. Duplicate pairs are rejected. Target strings are trimmed, nonempty UTF-8 and at most 1024 bytes each; provider names have the same byte bound, while model names retain their 128-Unicode-character bound. Unknown strategies and invalid target lists return 400; missing provider references return 404. Every target protects its provider from deletion.

PUT accepts optional positive `expectedVersion` (or its alias `version`; both must agree when supplied). The comparison and complete route replacement occur atomically in the write transaction. A stale version returns 409 without changing targets, metadata, name or revision. Missing versions retain legacy unconditional replacement. Creation starts at version 1; successful replacements increment it once. Writes never create model grants automatically. The embedded WebUI remains a legacy single-target editor; ordered-route editing is API-only. Runtime cooldown observations are in memory, never configuration rows; see [routing runtime](routing-runtime.md).

A PUT that omits `strategy` is accepted only when the stored route is still
`single`. An atomic strategy predicate in the same replacement transaction rejects
ambiguous legacy edits of fallback routes with 409 (`strategy_required`), leaving
the full route and revision unchanged. This prevents the single-target WebUI from
silently losing ordered targets, including when another client changes strategy
concurrently. To intentionally replace a fallback route, clients must explicitly
specify `strategy` (including an intentional downgrade to `single`).

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
SELECT model_id, position, provider_id, upstream_model FROM model_targets ORDER BY model_id, position;
SELECT account_id, model_id, created_at FROM model_grants ORDER BY account_id, model_id;
SELECT id, account_id, display_hint, created_at, revoked_at FROM local_keys ORDER BY account_id, created_at;
```

Do not select or export rows from `provider_credentials` or the `key_hash` column in `local_keys`; those columns contain authentication material. `foreign_keys` and `busy_timeout` are connection settings, so a separate SQLite shell does not report the application's connection values. WAL mode persists in the database and is visible to the shell.
