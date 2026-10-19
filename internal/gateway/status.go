package gateway

import (
	"context"
	"time"

	"github.com/gosuda/gyemoim/internal/config"
)

type RoutingStatus struct {
	ObservedAt time.Time            `json:"observedAt"`
	Models     []ModelRoutingStatus `json:"models"`
}

type ModelRoutingStatus struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Strategy          string         `json:"strategy"`
	ExecutionEnabled  bool           `json:"executionEnabled"`
	Targets           []TargetStatus `json:"targets"` // priority order
	PredictedTarget   *Target        `json:"predictedTarget,omitempty"`
	UnavailableReason string         `json:"unavailableReason,omitempty"`
	RetryAfterSeconds int64          `json:"retryAfterSeconds,omitempty"`
}

// RoutingStatus uses one brief non-secret configuration transaction, then one
// brief runtime lock. It does no provider I/O, reserves nothing, and is only a
// prediction: eligibility may change before the next selection.
func (s *Service) RoutingStatus(ctx context.Context) (RoutingStatus, error) {
	snapshot, err := s.store.SnapshotRouting(ctx)
	if err != nil {
		return RoutingStatus{}, err
	}
	return s.runtime.Snapshot(snapshot, time.Now()), nil
}

func (s *Service) PruneRuntime(ctx context.Context) error {
	snapshot, err := s.store.SnapshotRouting(ctx)
	if err != nil {
		return err
	}
	s.runtime.Prune(snapshot)
	return nil
}

func (r *Runtime) Snapshot(snapshot config.RoutingSnapshot, now time.Time) RoutingStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := RoutingStatus{ObservedAt: now.UTC(), Models: make([]ModelRoutingStatus, 0, len(snapshot.Models))}
	for _, model := range snapshot.Models {
		status := ModelRoutingStatus{ID: model.ID, Name: model.Name, Strategy: model.Strategy,
			ExecutionEnabled: model.Strategy == "single", Targets: make([]TargetStatus, 0, len(model.Targets))}
		var earliest time.Time
		for _, configured := range model.Targets {
			target := Target{ProviderID: configured.ProviderID, UpstreamModel: configured.UpstreamModel}
			state := r.statusLocked(target, snapshot.Providers[target.ProviderID], now)
			status.Targets = append(status.Targets, state)
			eligible := state.Status == "eligible" || model.Strategy == "single" && state.Status == "cooling"
			if status.PredictedTarget == nil && eligible && (model.Strategy == "single" || model.Strategy == "fallback") {
				chosen := target
				status.PredictedTarget = &chosen
			}
			if state.Status == "cooling" && state.EligibleAt != nil && (earliest.IsZero() || state.EligibleAt.Before(earliest)) {
				earliest = *state.EligibleAt
			}
		}
		if model.Strategy != "single" && model.Strategy != "fallback" {
			status.UnavailableReason = "unsupported_strategy"
		} else if status.PredictedTarget == nil {
			status.UnavailableReason = "all_disconnected"
			if !earliest.IsZero() {
				status.UnavailableReason = "all_cooling"
				status.RetryAfterSeconds = (&SelectionUnavailable{EligibleAt: earliest}).RetryAfterSeconds(now)
			}
		}
		result.Models = append(result.Models, status)
	}
	return result
}
