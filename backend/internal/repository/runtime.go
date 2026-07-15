package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const defaultRuntimeHeartbeat = 10 * time.Second
const minimumRuntimeInstanceStaleAfter = 30 * time.Second

type RuntimeInstanceLiveness struct {
	Exists            bool
	Fresh             bool
	HeartbeatAt       time.Time
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
}

func (s *Store) RegisterRuntimeInstance(ctx context.Context, instanceID string, heartbeat time.Duration) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return errors.New("runtime instance id is required")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO runtime_instances(instance_id,heartbeat_interval_ms)
		VALUES($1,$2)
		ON CONFLICT(instance_id) DO UPDATE
		SET heartbeat_at=now(),heartbeat_interval_ms=EXCLUDED.heartbeat_interval_ms,updated_at=now()`, instanceID, runtimeHeartbeatMillis(heartbeat))
	return err
}

func runtimeHeartbeatMillis(heartbeat time.Duration) int64 {
	if heartbeat <= 0 {
		heartbeat = defaultRuntimeHeartbeat
	}
	if milliseconds := heartbeat.Milliseconds(); milliseconds > 0 {
		return milliseconds
	}
	return 1
}

func (s *Store) HeartbeatRuntimeInstance(ctx context.Context, instanceID string) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return errors.New("runtime instance id is required")
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE runtime_instances
		SET heartbeat_at=now(),updated_at=now()
		WHERE instance_id=$1`, instanceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// A prior recovery pass may have removed an ancient liveness row while
		// this instance was disconnected. Re-register instead of silently losing
		// ownership evidence for all subsequently started CLI processes.
		return s.RegisterRuntimeInstance(ctx, instanceID, defaultRuntimeHeartbeat)
	}
	return nil
}

func (s *Store) UnregisterRuntimeInstance(ctx context.Context, instanceID string) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM runtime_instances WHERE instance_id=$1`, instanceID)
	return err
}

func runtimeInstanceLiveness(ctx context.Context, tx pgx.Tx, instanceID string, now time.Time) (RuntimeInstanceLiveness, error) {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return RuntimeInstanceLiveness{}, nil
	}
	var heartbeatAt time.Time
	var intervalMS int64
	err := tx.QueryRow(ctx, `SELECT heartbeat_at,heartbeat_interval_ms FROM runtime_instances WHERE instance_id=$1`, instanceID).Scan(&heartbeatAt, &intervalMS)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeInstanceLiveness{}, nil
	}
	if err != nil {
		return RuntimeInstanceLiveness{}, err
	}
	interval := time.Duration(intervalMS) * time.Millisecond
	staleAfter := interval * 3
	if staleAfter < minimumRuntimeInstanceStaleAfter {
		staleAfter = minimumRuntimeInstanceStaleAfter
	}
	return RuntimeInstanceLiveness{
		Exists: true, Fresh: !heartbeatAt.Before(now.Add(-staleAfter)),
		HeartbeatAt: heartbeatAt, HeartbeatInterval: interval, StaleAfter: staleAfter,
	}, nil
}
