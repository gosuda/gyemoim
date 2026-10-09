// In-memory tracker of successful local-key authentications. It mirrors the
// design philosophy of the httpapi rejection ring: the authentication hot path
// only performs a constant-time mutex-guarded map write (no allocation beyond
// the map entry itself), never touches the database, request recording, or the
// inference semaphore, and everything recorded is process-local — entries are
// lost at restart, which is acceptable because they are at most one flush
// interval (30 s) of staleness for a diagnostics-only timestamp. The
// config.Store persists a snapshot through UpdateKeysLastUsed on a periodic
// flush and best-effort at graceful shutdown; that store method is the only
// writer of local_keys.last_used_at.
package gateway

import (
	"sync"
	"time"
)

// KeyUsageTracker accumulates the most recent successful-authentication time
// per key ID between flushes. The zero value is usable; a nil
// *KeyUsageTracker silently drops records so callers need no existence checks
// on the request path.
type KeyUsageTracker struct {
	mu       sync.Mutex
	lastUsed map[string]time.Time
}

// NewKeyUsageTracker returns an empty usage tracker.
func NewKeyUsageTracker() *KeyUsageTracker {
	return &KeyUsageTracker{lastUsed: make(map[string]time.Time)}
}

// Record notes one successful key authentication. It is constant time and
// allocates nothing beyond the map entry. Only successful authentication may
// call it — failures must not record. Because concurrent requests can commit
// their times out of order, a record only ever moves a key's in-memory time
// forward, so a flush can never regress the stored timestamp.
func (t *KeyUsageTracker) Record(keyID string, at time.Time) {
	if t == nil || keyID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing, ok := t.lastUsed[keyID]; ok && !at.After(existing) {
		return
	}
	t.lastUsed[keyID] = at
}

// TakePending atomically removes and returns the pending entries so a flush
// writes each key once per interval instead of rewriting its full history.
// Records that race with the take land in the fresh map and flush on the next
// tick. A failed flush re-records its entries through Record, whose
// forward-only rule keeps any racing newer time intact.
func (t *KeyUsageTracker) TakePending() map[string]time.Time {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	taken := t.lastUsed
	t.lastUsed = make(map[string]time.Time)
	return taken
}
