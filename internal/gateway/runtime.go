package gateway

import (
	"sync"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
)

// Runtime owns only process-local observations, keyed by provider/upstream pair,
// never by alias, priority or request. Connection status remains config-owned.
type Runtime struct {
	mu      sync.Mutex
	seq     uint64
	entries map[Target]*runtimeEntry
}

type runtimeEntry struct {
	eligibleAt   time.Time
	delayReason  string
	delaySource  string
	lastFailure  *ObservedFailure
	last429      *ObservedFailure
	failureSeq   uint64
	successStart uint64
}

type ObservedFailure struct {
	At          time.Time  `json:"at"`
	Reason      string     `json:"reason"`
	HTTPStatus  int        `json:"httpStatus,omitempty"`
	EligibleAt  *time.Time `json:"eligibleAt,omitempty"`
	DelaySource string     `json:"delaySource,omitempty"`
}

// AttemptObservation is a one-use observation token, begun immediately before
// provider authentication/work, not at selection. It is not retained by Runtime.
type AttemptObservation struct {
	runtime *Runtime
	target  Target
	entry   *runtimeEntry
	start   uint64
	done    bool // guarded by runtime.mu
}

func NewRuntime() *Runtime { return &Runtime{entries: make(map[Target]*runtimeEntry)} }

func (r *Runtime) BeginAttempt(target Target) *AttemptObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[Target]*runtimeEntry)
	}
	e := r.entries[target]
	if e == nil {
		e = &runtimeEntry{}
		r.entries[target] = e
	}
	r.seq++
	return &AttemptObservation{runtime: r, target: target, entry: e, start: r.seq}
}

// ObserveFailure returns the same policy used for selection continuation.
// Failures are observed in completion order and can only extend a cooldown.
// Deleted/pruned tokens cannot recreate entries. No raw error is accepted.
func (r *Runtime) ObserveFailure(token *AttemptObservation, failure Failure, now time.Time) FailurePolicy {
	p := ClassifyFailure(failure, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.consume(token) {
		return p
	}
	// Local/client failures must never poison target health. A non-retryable
	// upstream rejection remains diagnostic but does not establish a cooldown.
	if p.Cooldown == 0 && p.DelaySource == "" {
		if failure.Kind == FailureHTTP && p.HTTPStatus >= 400 || failure.Kind == FailureInitialStream {
			r.seq++
			token.entry.failureSeq = r.seq
			token.entry.lastFailure = &ObservedFailure{At: now.UTC(), Reason: p.Reason, HTTPStatus: p.HTTPStatus}
		}
		return p
	}
	r.seq++
	e := token.entry
	e.failureSeq = r.seq
	until := now.Add(p.Cooldown)
	utcUntil := until.UTC()
	observation := &ObservedFailure{At: now.UTC(), Reason: p.Reason, HTTPStatus: p.HTTPStatus, EligibleAt: &utcUntil, DelaySource: p.DelaySource}
	e.lastFailure = observation
	if p.HTTPStatus == 429 {
		e.last429 = observation
	}
	if !until.Before(e.eligibleAt) {
		e.eligibleAt, e.delayReason, e.delaySource = until, p.Reason, p.DelaySource
	}
	return p
}

// ObserveSuccess clears a cooldown only if the attempt BEGAN after every
// observed failure. An old success cannot erase a newer concurrent failure.
// Success never erases historical failure/429 diagnostics.
func (r *Runtime) ObserveSuccess(token *AttemptObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.consume(token) {
		return
	}
	e := token.entry
	if token.start > e.failureSeq && token.start > e.successStart {
		e.successStart = token.start
		e.eligibleAt, e.delayReason, e.delaySource = time.Time{}, "", ""
	}
}

func (r *Runtime) consume(token *AttemptObservation) bool {
	if token == nil || token.runtime != r || token.done {
		return false
	}
	token.done = true
	return r.entries[token.target] == token.entry
}

type TargetStatus struct {
	Target
	Status      string           `json:"status"` // disconnected, cooling, eligible (not healthy)
	EligibleAt  *time.Time       `json:"eligibleAt,omitempty"`
	Reason      string           `json:"reason,omitempty"`
	DelaySource string           `json:"delaySource,omitempty"`
	LastFailure *ObservedFailure `json:"lastFailure,omitempty"`
	Last429     *ObservedFailure `json:"last429,omitempty"`
}

func (r *Runtime) Status(target Target, provider config.Provider, now time.Time) TargetStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusLocked(target, provider, now)
}

func (r *Runtime) statusLocked(target Target, provider config.Provider, now time.Time) TargetStatus {
	s := TargetStatus{Target: target, Status: "eligible"}
	if e := r.entries[target]; e != nil {
		if !e.eligibleAt.IsZero() {
			until := e.eligibleAt.UTC()
			s.EligibleAt, s.Reason, s.DelaySource = &until, e.delayReason, e.delaySource
			if now.Before(until) {
				s.Status = "cooling"
			}
		}
		// Copy before releasing the lock; callers cannot mutate runtime state.
		if e.lastFailure != nil {
			s.LastFailure = cloneObservedFailure(e.lastFailure)
		}
		if e.last429 != nil {
			s.Last429 = cloneObservedFailure(e.last429)
		}
	}
	if provider.ID != target.ProviderID || provider.Status != "connected" {
		s.Status, s.Reason = "disconnected", "disconnected"
	}
	return s
}

func cloneObservedFailure(failure *ObservedFailure) *ObservedFailure {
	copy := *failure
	if failure.EligibleAt != nil {
		until := *failure.EligibleAt
		copy.EligibleAt = &until
	}
	return &copy
}

// Prune removes deleted/unreferenced pairs. Call periodically with a fresh full
// config snapshot, never an individual alias (pairs are shared across aliases).
// Entries grow with configured destinations, not request count; no attempt list
// is retained. A late completion for a pruned entry is ignored.
func (r *Runtime) Prune(snapshot config.RoutingSnapshot) {
	active := make(map[Target]struct{})
	for _, model := range snapshot.Models {
		for _, target := range model.Targets {
			active[Target{ProviderID: target.ProviderID, UpstreamModel: target.UpstreamModel}] = struct{}{}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for target := range r.entries {
		if _, found := active[target]; !found {
			delete(r.entries, target)
		}
	}
}
