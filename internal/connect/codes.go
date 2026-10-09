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
	// enrollmentTTL bounds both unclaimed and claimed codes. Claimed codes get
	// a fresh TTL at claim time, which is also when the underlying OAuth flow
	// (its own 10-minute pending lifetime) starts, so a claimed code never
	// expires before its flow does.
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
	claimed    bool
	state      string // recorded at claim time; empty until the flow starts
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
// unclaimed code for the same provider. Claimed codes stay until their flow
// completes or they expire.
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
	for existing, record := range c.codes {
		if record.providerID == providerID && !record.claimed {
			delete(c.codes, existing)
		}
	}
	if len(c.codes) >= maxCodes {
		return "", time.Time{}, errEnrollmentUnavailable
	}
	c.codes[code] = enrollmentRecord{providerID: providerID, expiresAt: expiresAt}
	return code, expiresAt, nil
}

// claim consumes a live unclaimed code (constant-time compare so response
// timing cannot probe codes) and returns its provider ID. The record stays
// behind, marked claimed, so the completion step can find the flow's state.
func (c *codeStore) claim(code string, now time.Time) (string, bool) {
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
		if record.claimed || !now.Before(record.expiresAt) {
			return "", false
		}
		// Reset the TTL at claim time: this is when the OAuth flow starts.
		record.claimed = true
		record.expiresAt = now.Add(enrollmentTTL)
		c.codes[existing] = record
		return record.providerID, true
	}
	return "", false
}

// recordState links a claimed code to the OAuth flow state it started.
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
		if !record.claimed || !now.Before(record.expiresAt) {
			return false
		}
		record.state = state
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
		if !now.Before(record.expiresAt) {
			delete(c.codes, code)
		}
	}
}
