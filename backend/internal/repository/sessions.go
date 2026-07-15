package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

type CreateFeedbackRevisionIntakeParams struct {
	ProjectID                  uuid.UUID
	FeedbackID                 uuid.UUID
	Title                      string
	Body                       string
	ConfigSnapshot             json.RawMessage
	QueuePlan                  bool
	RequirementSessionID       string
	RequirementSessionProvider string
}

// GetActiveRequirementSession returns a resumable requirement session only
// when every isolation dimension matches. Callers must not resume a CLI session
// obtained through a looser lookup.
func (s *Store) GetActiveRequirementSession(ctx context.Context, projectID, intakeID uuid.UUID, provider string) (domain.AgentSession, error) {
	return scanAgentSession(s.Pool.QueryRow(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions
		WHERE project_id=$1 AND intake_id=$2 AND provider=$3 AND purpose='requirement' AND status='active'
		  AND length(trim(cli_session_id)) > 0`, projectID, intakeID, strings.TrimSpace(provider)))
}

// GetActiveExecutionSession returns a resumable execution session only when
// project, provider, purpose, scope, and status all match.
func (s *Store) GetActiveExecutionSession(ctx context.Context, projectID, planID uuid.UUID, provider string) (domain.AgentSession, error) {
	return scanAgentSession(s.Pool.QueryRow(ctx, `SELECT `+agentSessionColumns+` FROM agent_sessions
		WHERE project_id=$1 AND plan_id=$2 AND provider=$3 AND purpose='execution' AND status='active'
		  AND length(trim(cli_session_id)) > 0`, projectID, planID, strings.TrimSpace(provider)))
}

// GetFeedbackTraceForRevisionIntake resolves the immutable source feedback for
// a derived revision intake without exposing a cross-project relationship.
func (s *Store) GetFeedbackTraceForRevisionIntake(ctx context.Context, projectID, revisionIntakeID uuid.UUID) (domain.FeedbackTrace, error) {
	var feedbackID, actualProject uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT feedback_id,project_id FROM feedback_revisions
		WHERE revision_intake_id=$1 ORDER BY created_at,id LIMIT 1`, revisionIntakeID).Scan(&feedbackID, &actualProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FeedbackTrace{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.FeedbackTrace{}, err
	}
	if actualProject != projectID {
		return domain.FeedbackTrace{}, domain.ErrForbidden
	}
	return s.GetFeedbackTrace(ctx, projectID, feedbackID)
}

// GetFeedbackTraceForRevisionPlan resolves feedback from the generated
// revision-plan side of the append-only relationship.
func (s *Store) GetFeedbackTraceForRevisionPlan(ctx context.Context, projectID, revisionPlanID uuid.UUID) (domain.FeedbackTrace, error) {
	var feedbackID, actualProject uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT fr.feedback_id,frp.project_id
		FROM feedback_revision_plans frp
		JOIN feedback_revisions fr ON fr.id=frp.feedback_revision_id
		WHERE frp.revision_plan_id=$1 ORDER BY frp.created_at,frp.id LIMIT 1`, revisionPlanID).Scan(&feedbackID, &actualProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.FeedbackTrace{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.FeedbackTrace{}, err
	}
	if actualProject != projectID {
		return domain.FeedbackTrace{}, domain.ErrForbidden
	}
	return s.GetFeedbackTrace(ctx, projectID, feedbackID)
}

// CreateFeedbackRevisionIntake atomically creates a standalone requirement
// intake, records its immutable feedback source, persists the optional read-only
// discussion session, and uses the existing plan-generation queue path.
func (s *Store) CreateFeedbackRevisionIntake(ctx context.Context, p CreateFeedbackRevisionIntakeParams) (domain.Intake, *domain.Job, domain.FeedbackRevision, error) {
	p.Title = strings.TrimSpace(p.Title)
	if p.Title == "" {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, errors.New("revision title is required")
	}
	if len(p.ConfigSnapshot) == 0 {
		p.ConfigSnapshot = json.RawMessage(`{}`)
	}
	if sessionID := strings.TrimSpace(p.RequirementSessionID); sessionID != "" {
		if p.RequirementSessionProvider != "codex" && p.RequirementSessionProvider != "claude" {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, errors.New("invalid requirement session provider")
		}
		p.RequirementSessionID = sessionID
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}
	defer tx.Rollback(ctx)

	var linkProject, requirementID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT project_id,requirement_id FROM feedback_links WHERE feedback_id=$1 FOR SHARE`, p.FeedbackID).Scan(&linkProject, &requirementID)
	if errors.Is(err, pgx.ErrNoRows) {
		var feedbackProject uuid.UUID
		lookupErr := tx.QueryRow(ctx, `SELECT project_id FROM intakes WHERE id=$1 AND kind='feedback'`, p.FeedbackID).Scan(&feedbackProject)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, domain.ErrNotFound
		}
		if lookupErr != nil {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, lookupErr
		}
		if feedbackProject != p.ProjectID {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, domain.ErrForbidden
		}
		return domain.Intake{}, nil, domain.FeedbackRevision{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}
	if linkProject != p.ProjectID {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, domain.ErrForbidden
	}

	status := "open"
	if p.QueuePlan {
		status = "planning"
	}
	intakeID := uuid.New()
	var intake domain.Intake
	err = tx.QueryRow(ctx, `INSERT INTO intakes(id,project_id,kind,title,body,status,config_snapshot)
		VALUES($1,$2,'requirement',$3,$4,$5,$6)
		RETURNING id,project_id,kind,parent_intake_id,title,body,status,config_snapshot,created_at,updated_at,version`,
		intakeID, p.ProjectID, p.Title, p.Body, status, p.ConfigSnapshot).Scan(
		&intake.ID, &intake.ProjectID, &intake.Kind, &intake.ParentIntakeID, &intake.Title, &intake.Body,
		&intake.Status, &intake.ConfigSnapshot, &intake.CreatedAt, &intake.UpdatedAt, &intake.Version)
	if err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}
	if _, err = insertEvent(ctx, tx, NewEvent{ProjectID: &p.ProjectID, Type: "intake.created", AggregateType: "intake", AggregateID: intake.ID, ResourceVersion: intake.Version, Payload: mustJSON(map[string]any{"kind": "requirement", "status": status, "sourceFeedbackId": p.FeedbackID})}); err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}

	if p.RequirementSessionID != "" {
		if _, err = tx.Exec(ctx, `INSERT INTO agent_sessions(id,project_id,intake_id,provider,purpose,cli_session_id,context_summary,status)
			VALUES($1,$2,$3,$4,'requirement',$5,$6,'active')`, uuid.New(), p.ProjectID, intake.ID,
			p.RequirementSessionProvider, p.RequirementSessionID, truncateIntakeSessionSummary(intake.Title, intake.Body)); err != nil {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, err
		}
	}

	revision := domain.FeedbackRevision{
		ID: uuid.New(), FeedbackID: p.FeedbackID, ProjectID: p.ProjectID, RequirementID: requirementID,
		RevisionIntake: intake,
	}
	if err = tx.QueryRow(ctx, `INSERT INTO feedback_revisions(id,feedback_id,project_id,requirement_id,revision_intake_id)
		VALUES($1,$2,$3,$4,$5) RETURNING created_at`, revision.ID, revision.FeedbackID, revision.ProjectID,
		revision.RequirementID, intake.ID).Scan(&revision.CreatedAt); err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}

	var job *domain.Job
	if p.QueuePlan {
		maxAttempts, maxErr := projectMaxAttempts(ctx, tx, p.ProjectID)
		if maxErr != nil {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, maxErr
		}
		provider, _ := executionProviderFromContext(ctx)
		queued, queueErr := insertJob(ctx, tx, NewJob{
			ID: uuid.New(), ProjectID: p.ProjectID, Type: "plan.generate", AggregateType: "intake", AggregateID: intake.ID,
			Payload: planGenerationJobPayload(intake.ID, provider), Priority: 100, MaxAttempts: maxAttempts,
			RunAfter: time.Now(), IdempotencyKey: fmt.Sprintf("plan.generate:%s:%d", intake.ID, intake.Version),
		})
		if queueErr != nil {
			return domain.Intake{}, nil, domain.FeedbackRevision{}, queueErr
		}
		job = &queued
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Intake{}, nil, domain.FeedbackRevision{}, err
	}
	return intake, job, revision, nil
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
