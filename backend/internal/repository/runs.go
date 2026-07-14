package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

type AgentRunStart struct {
	ID, ProjectID                     uuid.UUID
	JobID, TaskID                     *uuid.UUID
	Provider, CommandSummary, LogPath string
	OwnerInstanceID                   string
}

func (s *Store) StartAgentRun(ctx context.Context, p AgentRunStart) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO agent_runs(id,project_id,job_id,task_id,provider,command_summary,status,log_path,owner_instance_id) VALUES($1,$2,$3,$4,$5,$6,'running',$7,$8)`, p.ID, p.ProjectID, p.JobID, p.TaskID, p.Provider, p.CommandSummary, p.LogPath, p.OwnerInstanceID)
	return err
}
func (s *Store) SetAgentRunPID(ctx context.Context, id uuid.UUID, pid int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET pid=$2,updated_at=now(),version=version+1 WHERE id=$1 AND status='running'`, id, pid)
	return err
}
func (s *Store) FinishAgentRun(ctx context.Context, id uuid.UUID, status string, exitCode int, sessionID, reason string, duration time.Duration) error {
	// Cancellation is persisted transactionally when automation is stopped. Do
	// not let a late process-exit callback overwrite that terminal state.
	_, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET status=$2,exit_code=$3,session_id=nullif($4,''),termination_reason=nullif($5,''),duration_ms=$6,finished_at=now(),updated_at=now(),version=version+1 WHERE id=$1 AND status='running'`, id, status, exitCode, sessionID, reason, duration.Milliseconds())
	return err
}

const agentRunColumns = `id,project_id,job_id,task_id,provider,command_summary,pid,coalesce(session_id,''),status,exit_code,duration_ms,log_path,coalesce(termination_reason,''),started_at,finished_at,created_at,updated_at,version`

func scanAgentRun(row pgx.Row) (domain.AgentRun, error) {
	var run domain.AgentRun
	err := row.Scan(&run.ID, &run.ProjectID, &run.JobID, &run.TaskID, &run.Provider, &run.CommandSummary, &run.PID, &run.SessionID, &run.Status, &run.ExitCode, &run.DurationMS, &run.LogPath, &run.TerminationReason, &run.StartedAt, &run.FinishedAt, &run.CreatedAt, &run.UpdatedAt, &run.Version)
	if err == pgx.ErrNoRows {
		err = domain.ErrNotFound
	}
	return run, err
}

func (s *Store) GetAgentRun(ctx context.Context, id uuid.UUID) (domain.AgentRun, error) {
	return scanAgentRun(s.Pool.QueryRow(ctx, `SELECT `+agentRunColumns+` FROM agent_runs WHERE id=$1`, id))
}

func (s *Store) ListAgentRuns(ctx context.Context, projectID uuid.UUID, limit int) ([]domain.AgentRun, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+agentRunColumns+` FROM agent_runs WHERE project_id=$1 ORDER BY created_at DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.AgentRun, 0)
	for rows.Next() {
		run, scanErr := scanAgentRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, run)
	}
	return items, rows.Err()
}
