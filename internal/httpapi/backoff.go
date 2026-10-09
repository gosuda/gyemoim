// In-memory per-username exponential backoff for failed management logins
// (decision 8 in docs/web-deployment.md). State lives only in this process:
// restarting the server resets it, which is acceptable for a personal tool.
// There is deliberately no IP dimension — client IPs are not trusted
// (decision 9) — and no persistence. On success the username's state is
// cleared; the change-password path needs no backoff because it already
// requires a valid session.
package httpapi

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	// Backoff policy for repeated login failures on the same username. After
	// n consecutive real failures the username is blocked for
	// baseDelay * 2^(n-1): 1 s, 2 s, 4 s, ... capped at maxDelay. Numbers are
	// deliberately gentle — this is a low-traffic personal tool where the
	// backoff only has to make untargeted brute force pointless; nginx rate
	// limiting remains the outer line of defense.
	loginBackoffBaseDelay = 1 * time.Second
	loginBackoffMaxDelay  = 15 * time.Minute

	// Memory bound: entries are swept when they have been out of their backoff
	// window for retainAfterWindow, and the map is additionally capped at
	// maxEntries (evicting the oldest failure first). Both bounds are generous
	// for a personal tool; every entry is ~100 bytes.
	loginBackoffRetainAfterWindow = 15 * time.Minute
	loginBackoffMaxEntries        = 1024
)

// loginBackoff tracks consecutive login failures per username. A username that
// was never observed failing has no entry, so the common case costs nothing.
type loginBackoff struct {
	mu      sync.Mutex
	entries map[string]loginFailure
}

type loginFailure struct {
	failures     int
	blockedUntil time.Time
	lastFailure  time.Time
}

// blocked reports whether username is inside a backoff window. Requests
// rejected here do not touch the entry at all: spamming during a window stays
// cheap and does not extend or reset it, so an attacker cannot lock an account
// forever — the window decays naturally once they stop.
func (b *loginBackoff) blocked(username string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[username]
	return ok && now.Before(entry.blockedUntil)
}

// recordFailure counts one real failed attempt (one that got past the backoff
// check and paid for an argon2id verification) and opens the next backoff
// window. It also opportunistically expires stale entries and enforces the
// entry cap, so the map cannot grow without bound.
func (b *loginBackoff) recordFailure(username string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweep(now)
	entry := b.entries[username]
	entry.failures++
	entry.lastFailure = now
	entry.blockedUntil = now.Add(loginBackoffDelay(entry.failures))
	if b.entries == nil {
		b.entries = make(map[string]loginFailure)
	}
	b.entries[username] = entry
	// Decision 8: real failures go to stderr (never to history). Backoff
	// rejects are not logged, so spamming produces at most one line per window.
	fmt.Fprintf(os.Stderr, "gyemoim: failed management login for %q (consecutive failure %d, next attempt blocked for %s)\n",
		username, entry.failures, entry.blockedUntil.Sub(now).Round(time.Second))
}

// recordSuccess clears the username's failure state after a successful login.
func (b *loginBackoff) recordSuccess(username string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, username)
}

// loginBackoffDelay returns the backoff window for n consecutive failures:
// base doubled per failure, capped at maxDelay. The loop avoids overflow for
// any failure count a uint-backed map could hold.
func loginBackoffDelay(failures int) time.Duration {
	delay := loginBackoffBaseDelay
	for i := 1; i < failures && delay < loginBackoffMaxDelay; i++ {
		delay *= 2
	}
	if delay > loginBackoffMaxDelay {
		return loginBackoffMaxDelay
	}
	return delay
}

// sweep drops entries whose backoff window ended more than
// retainAfterWindow ago; if the map is still over the cap, it evicts the
// oldest failures. Callers must hold b.mu.
func (b *loginBackoff) sweep(now time.Time) {
	for username, entry := range b.entries {
		if now.After(entry.blockedUntil.Add(loginBackoffRetainAfterWindow)) {
			delete(b.entries, username)
		}
	}
	for len(b.entries) > loginBackoffMaxEntries {
		oldest, oldestTime, found := "", time.Time{}, false
		for username, entry := range b.entries {
			if !found || entry.lastFailure.Before(oldestTime) {
				oldest, oldestTime, found = username, entry.lastFailure, true
			}
		}
		if !found {
			return
		}
		delete(b.entries, oldest)
	}
}
