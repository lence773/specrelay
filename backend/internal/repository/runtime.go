package repository

import (
	"context"
	"errors"
	"strings"
	"time"
)

const defaultRuntimeHeartbeat = 10 * time.Second

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
	_, err := s.Pool.Exec(ctx, `UPDATE runtime_instances
		SET heartbeat_at=now(),updated_at=now()
		WHERE instance_id=$1`, instanceID)
	return err
}

func (s *Store) UnregisterRuntimeInstance(ctx context.Context, instanceID string) error {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM runtime_instances WHERE instance_id=$1`, instanceID)
	return err
}
