// Package connect implements the remote enrollment flow for headless servers:
// single-use enrollment codes, the script-facing claim/complete endpoints, and
// the embedded Python connect script. See docs/web-deployment.md decision 7.
package connect

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

const (
	// enrollmentTTL bounds both unclaimed and claimed codes. The claimed
	// code's fresh window starts at recordState time, when the underlying
	// OAuth flow (its own 10-minute pending lifetime) actually exists, so a
	// claimed code never expires before its flow does.
	enrollmentTTL = 10 * time.Minute
	// maxCodes bounds memory; expired entries are swept before the cap is
	// enforced, matching the siwc pending-flow behavior.
	maxCodes = 128
	// codeBytes yields a 128-bit random code, encoded as 22 base64url characters.
	codeBytes  = 16
	maxCodeLen = 128
)

type enrollmentRecord struct {
	providerID string
	reserved   bool   // a claim start is in flight; the code is unusable meanwhile
	claimed    bool   // the start succeeded and the flow state is linked
	state      string // empty until the flow starts
	expiresAt  time.Time
}

// codeStore is the in-memory enrollment code registry. Codes are process-local
// by design: a restart drops them (documented in docs/web-deployment.md) and
// the admin simply issues a new one.
type codeStore struct {
	mu    sync.Mutex
	codes map[string]enrollmentRecord
}

func newCodeStore() *codeStore {
	return &codeStore{codes: make(map[string]enrollmentRecord)}
}

// issue creates a fresh code for the provider and invalidates any earlier
// unclaimed, unreserved code for the same provider. Claimed codes stay until
// their flow completes or they expire. When the registry is full the call has
// no side effects: nothing is invalidated and no code is issued.
func (c *codeStore) issue(providerID string, now time.Time) (string, time.Time, error) {
	value := make([]byte, codeBytes)
	if _, err := rand.Read(value); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(value)
	expiresAt := now.Add(enrollmentTTL)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	if len(c.codes) >= maxCodes {
		return "", time.Time{}, ErrEnrollmentUnavailable
	}
	for existing, record := range c.codes {
		if record.providerID == providerID && !record.claimed && !record.reserved {
			delete(c.codes, existing)
		}
	}
	c.codes[code] = enrollmentRecord{providerID: providerID, expiresAt: expiresAt}
	return code, expiresAt, nil
}

// reserve atomically marks a live unclaimed code as reserved for an in-flight
// start and returns its provider ID. A reserved code cannot be claimed again
// or completed, and it is not swept, because its claim goroutine always ends
// by either recording the started flow (recordState) or releasing it
// (release). Until then it keeps its original expiry.
func (c *codeStore) reserve(code string, now time.Time) (string, bool) {
	if code == "" || len(code) > maxCodeLen {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	if len(c.codes) == 0 {
		return "", false
	}
	for existing, record := range c.codes {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(code)) != 1 {
			continue
		}
		if record.reserved || record.claimed || !now.Before(record.expiresAt) {
			return "", false
		}
		record.reserved = true
		c.codes[existing] = record
		return record.providerID, true
	}
	return "", false
}

// release undoes a reservation after a failed start so the same code can be
// claimed again. It is a no-op for a code that was already recorded claimed,
// and a released code that is never reclaimed still expires by its original
// TTL.
func (c *codeStore) release(code string) {
	if code == "" || len(code) > maxCodeLen {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for existing, record := range c.codes {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(code)) != 1 {
			continue
		}
		if record.reserved && !record.claimed {
			record.reserved = false
			c.codes[existing] = record
		}
		return
	}
}

// recordState atomically marks a reserved code claimed and links it to the
// OAuth flow state its start produced. The claimed code's fresh completion
// window starts here — when the flow actually exists — not at reserve time,
// so the code cannot expire while its flow is still live.
func (c *codeStore) recordState(code, state string, now time.Time) bool {
	if code == "" || len(code) > maxCodeLen || state == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for existing, record := range c.codes {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(code)) != 1 {
			continue
		}
		if !record.reserved || record.claimed {
			return false
		}
		record.claimed = true
		record.reserved = false
		record.state = state
		record.expiresAt = now.Add(enrollmentTTL)
		c.codes[existing] = record
		return true
	}
	return false
}

// claimedState returns the flow state recorded for a claimed code.
func (c *codeStore) claimedState(code string, now time.Time) (string, bool) {
	if code == "" || len(code) > maxCodeLen {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	for existing, record := range c.codes {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(code)) != 1 {
			continue
		}
		if !record.claimed || record.state == "" || !now.Before(record.expiresAt) {
			return "", false
		}
		return record.state, true
	}
	return "", false
}

// finish drops a code (and its claimed-flow link) so it can never be completed
// again, whatever the completion outcome was.
func (c *codeStore) finish(code string) {
	if code == "" || len(code) > maxCodeLen {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for existing := range c.codes {
		if subtle.ConstantTimeCompare([]byte(existing), []byte(code)) == 1 {
			delete(c.codes, existing)
			return
		}
	}
}

func (c *codeStore) sweepLocked(now time.Time) {
	for code, record := range c.codes {
		// Reserved codes belong to an in-flight claim; their goroutine always
		// records or releases them, so they are never swept out from under it.
		if record.reserved || now.Before(record.expiresAt) {
			continue
		}
		delete(c.codes, code)
	}
}
