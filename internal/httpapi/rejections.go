// In-memory ring of the last pre-admission harness rejections (plan decision 9c).
// Rejections that happen before the recorder admits a request — bad keys, denied
// model access, unknown models — never reach request history, so they previously
// vanished. The ring is the diagnostics surface for exactly those events: it
// records only after the rejection decision, never blocks or gates inference
// beyond the constant-time mutex-guarded push, allocates nothing on that push,
// and never touches request recording, history files, or the inference
// semaphore. Entries are process-local and reset at restart by design.
package httpapi

import (
	"sync"
	"time"
)

// rejectionLogCapacity bounds the ring. Once full, Record overwrites the oldest
// entry, so the buffer always holds the most recent rejections regardless of
// request volume.
const rejectionLogCapacity = 100

// Rejection is one pre-admission harness rejection. KeyHint uses the same
// last-four-character style as the stored key display hint and never contains
// the full presented key; unknown fields are simply empty (for example, a
// malformed Authorization header yields no hint, and a rejection before
// authentication yields no service-account identity).
type Rejection struct {
	At                 time.Time `json:"at"`
	KeyHint            string    `json:"keyHint,omitempty"`
	Code               string    `json:"code"`
	Model              string    `json:"model,omitempty"`
	ServiceAccountID   string    `json:"serviceAccountId,omitempty"`
	ServiceAccountName string    `json:"serviceAccountName,omitempty"`
}

// RejectionLog is a mutex-guarded bounded ring of the most recent rejections.
// The zero value is usable; a nil *RejectionLog silently drops records so
// callers need no existence checks on the request path.
type RejectionLog struct {
	mu      sync.Mutex
	entries [rejectionLogCapacity]Rejection
	next    int
	count   int
}

// NewRejectionLog returns an empty rejection ring.
func NewRejectionLog() *RejectionLog {
	return &RejectionLog{}
}

// Record appends one rejection, overwriting the oldest entry once the ring is
// full. It copies the entry into the fixed-size array: constant time, no
// allocation, safe under concurrent rejections.
func (l *RejectionLog) Record(rejection Rejection) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = rejection
	l.next = (l.next + 1) % rejectionLogCapacity
	if l.count < rejectionLogCapacity {
		l.count++
	}
}

// Snapshot returns copies of the recorded rejections, newest first.
func (l *RejectionLog) Snapshot() []Rejection {
	if l == nil {
		return []Rejection{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Rejection, 0, l.count)
	for index := 0; index < l.count; index++ {
		position := (l.next - 1 - index + rejectionLogCapacity) % rejectionLogCapacity
		out = append(out, l.entries[position])
	}
	return out
}
