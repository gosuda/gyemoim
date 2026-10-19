package config

import (
	"context"
	"database/sql"
	"fmt"
)

// RoutingSnapshot is non-secret configuration for management diagnostics and
// runtime pruning. It is not an inference authorization boundary.
type RoutingSnapshot struct {
	Models    []Model
	Providers map[string]Provider
}

func (s *Store) SnapshotRouting(ctx context.Context) (RoutingSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RoutingSnapshot{}, fmt.Errorf("begin routing snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := RoutingSnapshot{Models: make([]Model, 0), Providers: make(map[string]Provider)}
	rows, err := tx.QueryContext(ctx, `SELECT `+modelColumns+` FROM models m ORDER BY m.created_at, m.id`)
	if err != nil {
		return RoutingSnapshot{}, fmt.Errorf("list routing models: %w", err)
	}
	for rows.Next() {
		model, err := scanModel(rows)
		if err != nil {
			_ = rows.Close()
			return RoutingSnapshot{}, fmt.Errorf("read routing model: %w", err)
		}
		snapshot.Models = append(snapshot.Models, model)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return RoutingSnapshot{}, fmt.Errorf("list routing models: %w", err)
	}
	if closeErr != nil {
		return RoutingSnapshot{}, fmt.Errorf("close routing models: %w", closeErr)
	}
	rows, err = tx.QueryContext(ctx, `SELECT id, name, provider_type, base_url, status, created_at, updated_at FROM providers`)
	if err != nil {
		return RoutingSnapshot{}, fmt.Errorf("list routing providers: %w", err)
	}
	for rows.Next() {
		provider, err := scanProvider(rows)
		if err != nil {
			_ = rows.Close()
			return RoutingSnapshot{}, fmt.Errorf("read routing provider: %w", err)
		}
		snapshot.Providers[provider.ID] = provider
	}
	err = rows.Err()
	closeErr = rows.Close()
	if err != nil {
		return RoutingSnapshot{}, fmt.Errorf("list routing providers: %w", err)
	}
	if closeErr != nil {
		return RoutingSnapshot{}, fmt.Errorf("close routing providers: %w", closeErr)
	}
	if err := tx.Commit(); err != nil {
		return RoutingSnapshot{}, fmt.Errorf("commit routing snapshot: %w", err)
	}
	return snapshot, nil
}
