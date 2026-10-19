package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
)

var ErrDecisionRequired = errors.New("fallback requires decision-aware execution")
var ErrTargetsCooling = errors.New("routing targets are cooling down")
var ErrTargetsDisconnected = errors.New("routing targets are disconnected")

// SelectionUnavailable distinguishes no connected targets from cooling targets.
// EligibleAt is the earliest CONNECTED candidate's eligibility, not the maximum.
type SelectionUnavailable struct {
	Cause      error
	EligibleAt time.Time
}

func (e *SelectionUnavailable) Error() string { return e.Cause.Error() }
func (e *SelectionUnavailable) Unwrap() error { return e.Cause }
func (e *SelectionUnavailable) RetryAfterSeconds(now time.Time) int64 {
	if !e.EligibleAt.After(now) {
		return 0
	}
	delay := e.EligibleAt.Sub(now)
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return seconds
}

// SelectionDecision returns exactly one ordered candidate. Skip decisions MUST
// be durably recorded before asking for another candidate or doing provider I/O.
type SelectionDecision struct {
	Target   Target       `json:"target"`
	Position int          `json:"position"` // zero-based model priority
	Kind     string       `json:"kind"`     // attempt or skip
	State    TargetStatus `json:"state"`
}

// DecisionSelection is additive: existing single Selection.Next callers remain
// valid. Fallback.Next deliberately fails rather than discarding skip records.
type DecisionSelection interface {
	Selection
	NextDecision(context.Context, *AttemptOutcome) (SelectionDecision, error)
}

type Strategies struct{ Runtime *Runtime }

func (s Strategies) NewSelection(ctx context.Context, request RoutingRequest) (Selection, error) {
	switch request.Model.Strategy {
	case "single":
		selection, err := (SingleTargetStrategy{}).NewSelection(ctx, request)
		if err == nil {
			single := selection.(*singleSelection)
			single.runtime, single.provider = s.Runtime, request.Providers[single.target.ProviderID]
		}
		return selection, err
	case "fallback":
		return (FallbackStrategy{Runtime: s.Runtime}).NewSelection(ctx, request)
	default:
		return nil, ErrUnsupportedStrategy
	}
}

type FallbackStrategy struct{ Runtime *Runtime }

func (s FallbackStrategy) NewSelection(_ context.Context, request RoutingRequest) (Selection, error) {
	if request.Model.Strategy != "fallback" {
		return nil, ErrUnsupportedStrategy
	}
	if s.Runtime == nil || len(request.Model.Targets) == 0 {
		return nil, errors.New("fallback requires runtime and targets")
	}
	providers := make(map[string]config.Provider, len(request.Providers))
	for id, provider := range request.Providers {
		providers[id] = provider
	}
	targets := make([]Target, 0, len(request.Model.Targets))
	seen := make(map[Target]bool)
	for _, configured := range request.Model.Targets {
		target := Target{ProviderID: configured.ProviderID, UpstreamModel: configured.UpstreamModel}
		if target.ProviderID == "" || target.UpstreamModel == "" || seen[target] || providers[target.ProviderID].ID != target.ProviderID {
			return nil, errors.New("fallback has invalid targets or missing provider snapshot")
		}
		seen[target] = true
		targets = append(targets, target)
	}
	return &fallbackSelection{runtime: s.Runtime, targets: targets, providers: providers}, nil
}

type fallbackSelection struct {
	runtime       *Runtime
	targets       []Target
	providers     map[string]config.Provider
	position      int
	awaiting      bool
	attempted     bool
	stopped       bool
	firstEligible time.Time
}

func (*fallbackSelection) Next(context.Context, *AttemptOutcome) (Target, error) {
	return Target{}, ErrDecisionRequired
}

func (s *fallbackSelection) NextDecision(ctx context.Context, previous *AttemptOutcome) (SelectionDecision, error) {
	if err := ctx.Err(); err != nil {
		s.stopped = true
		return SelectionDecision{}, err
	}
	if s.stopped {
		return SelectionDecision{}, ErrSelectionExhausted
	}
	now := time.Now()
	if s.awaiting {
		// No guessed retries from legacy outcome strings, or status alone.
		if previous == nil || previous.Failure == nil || previous.ResponseCommitted || previous.ClientCanceled || previous.RecordingFailed {
			s.stopped = true
			return SelectionDecision{}, ErrSelectionExhausted
		}
		if !ClassifyFailure(*previous.Failure, now).Retryable {
			s.stopped = true
			return SelectionDecision{}, ErrSelectionExhausted
		}
	} else if previous != nil {
		// A skip is not an attempt. Cancellation/recording errors after a skip
		// stop locally; never treat them as permission to continue.
		s.stopped = true
		return SelectionDecision{}, ErrSelectionExhausted
	}
	s.awaiting = false
	if s.position == len(s.targets) {
		s.stopped = true
		if s.attempted {
			return SelectionDecision{}, ErrSelectionExhausted
		}
		if !s.firstEligible.IsZero() {
			return SelectionDecision{}, &SelectionUnavailable{Cause: ErrTargetsCooling, EligibleAt: s.firstEligible}
		}
		return SelectionDecision{}, &SelectionUnavailable{Cause: ErrTargetsDisconnected}
	}
	target := s.targets[s.position]
	state := s.runtime.Status(target, s.providers[target.ProviderID], now)
	decision := SelectionDecision{Target: target, Position: s.position, Kind: "skip", State: state}
	s.position++
	if state.Status == "eligible" {
		decision.Kind, s.awaiting, s.attempted = "attempt", true, true
	} else if state.Status == "cooling" && state.EligibleAt != nil {
		if s.firstEligible.IsZero() || state.EligibleAt.Before(s.firstEligible) {
			s.firstEligible = *state.EligibleAt
		}
	}
	return decision, nil
}

func (s *singleSelection) NextDecision(ctx context.Context, previous *AttemptOutcome) (SelectionDecision, error) {
	if err := ctx.Err(); err != nil {
		return SelectionDecision{}, err
	}
	target, err := s.Next(ctx, previous)
	if err != nil {
		return SelectionDecision{}, err
	}
	state := TargetStatus{Target: target, Status: "unobserved"}
	if s.runtime != nil {
		state = s.runtime.Status(target, s.provider, time.Now())
	}
	// Observed cooldown/disconnection is diagnostic only for single. Its
	// executor retains the existing connection/authentication checks.
	return SelectionDecision{Target: target, Kind: "attempt", State: state}, nil
}
