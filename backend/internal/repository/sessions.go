package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

const agentSessionColumns = `id,project_id,intake_id,plan_id,provider,purpose,cli_session_id,context_summary,last_task_id,status,created_at,updated_at,version`

func scanAgentSession(row pgx.Row) (domain.AgentSession, error) {
	var session domain.AgentSession
	err := row.Scan(
		&session.ID,
		&session.ProjectID,
		&session.IntakeID,
		&session.PlanID,
		&session.Provider,
		&session.Purpose,
		&session.CLISessionID,
		&session.ContextSummary,
		&session.LastTaskID,
		&session.Status,
		&session.CreatedAt,
		&session.UpdatedAt,
		&session.Version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		err = domain.ErrNotFound
	}
	return session, err
}

func (s *Store) GetRequirementSession(ctx context.Context, intakeID uuid.UUID) (domain.AgentSession, error) {
	return scanAgentSession(s.Pool.QueryRow(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions WHERE intake_id=$1 AND purpose='requirement'`, intakeID))
}

func (s *Store) GetExecutionSession(ctx context.Context, planID uuid.UUID) (domain.AgentSession, error) {
	return scanAgentSession(s.Pool.QueryRow(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions WHERE plan_id=$1 AND purpose='execution'`, planID))
}

func (s *Store) UpsertRequirementSession(ctx context.Context, projectID, intakeID uuid.UUID, provider, cliSessionID, summary string) (domain.AgentSession, error) {
	return s.upsertAgentSession(ctx, projectID, &intakeID, nil, provider, "requirement", cliSessionID, summary, nil, "active")
}

func (s *Store) UpsertExecutionSession(ctx context.Context, projectID, planID uuid.UUID, provider, cliSessionID, summary string, lastTaskID *uuid.UUID) (domain.AgentSession, error) {
	return s.upsertAgentSession(ctx, projectID, nil, &planID, provider, "execution", cliSessionID, summary, lastTaskID, "active")
}

func (s *Store) MarkAgentSessionStale(ctx context.Context, id uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agent_sessions SET status='stale',updated_at=now(),version=version+1 WHERE id=$1 AND status='active'`, id)
	return err
}

func (s *Store) upsertAgentSession(ctx context.Context, projectID uuid.UUID, intakeID, planID *uuid.UUID, provider, purpose, cliSessionID, summary string, lastTaskID *uuid.UUID, status string) (domain.AgentSession, error) {
	provider = strings.TrimSpace(provider)
	cliSessionID = strings.TrimSpace(cliSessionID)
	if provider != "codex" && provider != "claude" {
		return domain.AgentSession{}, errors.New("invalid agent session provider")
	}
	if cliSessionID == "" {
		return domain.AgentSession{}, errors.New("cli session id is required")
	}
	var query string
	var args []any
	if purpose == "requirement" && intakeID != nil {
		query = `INSERT INTO agent_sessions(id,project_id,intake_id,provider,purpose,cli_session_id,context_summary,last_task_id,status)
			VALUES($1,$2,$3,$4,'requirement',$5,$6,$7,$8)
			ON CONFLICT (intake_id) WHERE purpose='requirement' DO UPDATE
			SET provider=EXCLUDED.provider,cli_session_id=EXCLUDED.cli_session_id,context_summary=EXCLUDED.context_summary,last_task_id=EXCLUDED.last_task_id,status=EXCLUDED.status,updated_at=now(),version=agent_sessions.version+1
			RETURNING ` + agentSessionColumns
		args = []any{uuid.New(), projectID, *intakeID, provider, cliSessionID, summary, lastTaskID, status}
	} else if purpose == "execution" && planID != nil {
		query = `INSERT INTO agent_sessions(id,project_id,plan_id,provider,purpose,cli_session_id,context_summary,last_task_id,status)
			VALUES($1,$2,$3,$4,'execution',$5,$6,$7,$8)
			ON CONFLICT (plan_id) WHERE purpose='execution' DO UPDATE
			SET provider=EXCLUDED.provider,cli_session_id=EXCLUDED.cli_session_id,context_summary=EXCLUDED.context_summary,last_task_id=EXCLUDED.last_task_id,status=EXCLUDED.status,updated_at=now(),version=agent_sessions.version+1
			RETURNING ` + agentSessionColumns
		args = []any{uuid.New(), projectID, *planID, provider, cliSessionID, summary, lastTaskID, status}
	} else {
		return domain.AgentSession{}, errors.New("invalid agent session scope")
	}
	return scanAgentSession(s.Pool.QueryRow(ctx, query, args...))
}
