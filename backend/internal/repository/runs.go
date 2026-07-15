package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

type AgentRunStart struct {
	ID, ProjectID      uuid.UUID
	IntakeID, PlanID   *uuid.UUID
	JobID, TaskID      *uuid.UUID
	LogicalOperationID *uuid.UUID
	JobAttempt         *int
	RetryCount         *int
	QueueWaitMS        *int64

	Provider                  string
	OperationType             string
	CommandSummary            string
	SessionMode               string
	SessionInvalidationReason string
	LogPath                   string
	OwnerInstanceID           string
}

type AgentRunActivity struct {
	PID               *int
	ProcessIdentity   string
	HeartbeatAt       time.Time
	LastOutputAt      time.Time
	LogActivityAt     time.Time
	HeartbeatInterval time.Duration
}

type AgentRunFinish struct {
	Status                    string
	ExitCode                  *int
	SessionID                 string
	SessionMode               string
	SessionInvalidationReason string
	TerminationReason         string
	FailureCategory           string
	DurationMS                *int64
	OutputBytes               *int64
	OutputLines               *int64
	EventCount                *int64
	OutputTruncated           *bool
	InputTokens               *int64
	OutputTokens              *int64
	TotalTokens               *int64
	CostAmount                string
	CostCurrency              string
}

func (s *Store) StartAgentRun(ctx context.Context, p AgentRunStart) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO agent_runs(
		id,project_id,intake_id,plan_id,job_id,task_id,logical_operation_id,
		operation_type,job_attempt,retry_count,provider,command_summary,
		session_mode,session_invalidation_reason,queue_wait_ms,status,log_path,
		owner_instance_id,execution_checkpoint
	) VALUES(
		$1,$2,$3,$4,$5,$6,$7,
		nullif($8,''),$9,$10,$11,$12,
		nullif($13,''),nullif($14,''),$15,'running',$16,$17,
		jsonb_build_object('ownerInstanceId',$17::text,'phase','starting','heartbeatIntervalMs',5000)
	)`, p.ID, p.ProjectID, p.IntakeID, p.PlanID, p.JobID, p.TaskID, p.LogicalOperationID,
		p.OperationType, p.JobAttempt, p.RetryCount, p.Provider, p.CommandSummary,
		p.SessionMode, p.SessionInvalidationReason, p.QueueWaitMS, p.LogPath, p.OwnerInstanceID)
	return err
}

func (s *Store) SetAgentRunPID(ctx context.Context, id uuid.UUID, pid int) error {
	return s.UpdateAgentRunActivity(ctx, id, AgentRunActivity{PID: &pid, HeartbeatAt: time.Now().UTC()})
}

// UpdateAgentRunActivity persists compact process evidence without creating a
// lifecycle transition for every heartbeat or output fragment. Callers are
// expected to throttle high-frequency output updates; this method deliberately
// leaves the public resource version unchanged so observation traffic cannot
// manufacture business-state conflicts.
func (s *Store) UpdateAgentRunActivity(ctx context.Context, id uuid.UUID, activity AgentRunActivity) error {
	var pid any
	if activity.PID != nil && *activity.PID > 0 {
		pid = *activity.PID
	}
	var heartbeatAt, lastOutputAt, logActivityAt, heartbeatIntervalMS any
	if !activity.HeartbeatAt.IsZero() {
		heartbeatAt = activity.HeartbeatAt.UTC()
	}
	if !activity.LastOutputAt.IsZero() {
		lastOutputAt = activity.LastOutputAt.UTC()
	}
	if !activity.LogActivityAt.IsZero() {
		logActivityAt = activity.LogActivityAt.UTC()
	}
	if activity.HeartbeatInterval > 0 {
		heartbeatIntervalMS = runtimeHeartbeatMillis(activity.HeartbeatInterval)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET
		pid=coalesce($2::integer,pid),
		execution_checkpoint=execution_checkpoint || jsonb_strip_nulls(jsonb_build_object(
			'pid',$2::integer,'processIdentity',nullif($3,''),'heartbeatAt',$4::timestamptz,
			'lastOutputAt',$5::timestamptz,'logActivityAt',$6::timestamptz,
			'heartbeatIntervalMs',$7::bigint,'phase','running')),
		last_activity_at=GREATEST(last_activity_at,coalesce($4::timestamptz,'-infinity'::timestamptz),coalesce($5::timestamptz,'-infinity'::timestamptz),coalesce($6::timestamptz,'-infinity'::timestamptz)),
		updated_at=now()
		WHERE id=$1 AND status IN ('starting','running','cancelling')`, id, pid, activity.ProcessIdentity, heartbeatAt, lastOutputAt, logActivityAt, heartbeatIntervalMS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// FinishAgentRun retains the existing call surface for current execution code.
// New collectors should use FinishAgentRunWithDetails so absent CLI metrics stay
// NULL instead of being synthesized.
func (s *Store) FinishAgentRun(ctx context.Context, id uuid.UUID, status string, exitCode int, sessionID, reason string, duration time.Duration) error {
	exitCodeValue := exitCode
	durationMS := duration.Milliseconds()
	return s.FinishAgentRunWithDetails(ctx, id, AgentRunFinish{
		Status:            status,
		ExitCode:          &exitCodeValue,
		SessionID:         sessionID,
		TerminationReason: reason,
		DurationMS:        &durationMS,
	})
}

func (s *Store) FinishAgentRunWithDetails(ctx context.Context, id uuid.UUID, p AgentRunFinish) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var jobID *uuid.UUID
	var checkpoint json.RawMessage
	if err = tx.QueryRow(ctx, `SELECT job_id,execution_checkpoint FROM agent_runs WHERE id=$1`, id).Scan(&jobID, &checkpoint); errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}

	reasonCode := domain.LifecycleReasonExecutionFailed
	reason := strings.TrimSpace(p.TerminationReason)
	recoveryHint := domain.LifecycleRecoveryManualReview
	allowNonContract := false
	switch p.Status {
	case "succeeded":
		reasonCode = domain.LifecycleReasonCompleted
		reason = "agent run completed"
		recoveryHint = domain.LifecycleRecoveryNone
	case "failed":
		if reason == "" {
			reason = "agent run failed"
		}
	case "interrupted":
		reasonCode = domain.LifecycleReasonProcessLost
		if reason == "" {
			reason = "agent run was interrupted"
		}
		recoveryHint = domain.LifecycleRecoveryRetryFromStart
	case "cancelled":
		reasonCode = domain.LifecycleReasonCancellationRequested
		if reason == "" {
			reason = "agent run was cancelled"
		}
		recoveryHint = domain.LifecycleRecoveryNone
		// Existing process callbacks report cancellation directly from running.
		// The migration gate still supplies terminal protection and audit data.
		allowNonContract = true
	default:
		return domain.ErrInvalidTransition
	}

	var costAmount, costCurrency any
	if p.CostAmount != "" && p.CostCurrency != "" {
		costAmount = p.CostAmount
		costCurrency = p.CostCurrency
	}
	checkpoint = mergeAgentRunCheckpoint(checkpoint, map[string]any{
		"phase": "finished", "processExitedAt": time.Now().UTC(), "terminalStatus": p.Status,
	})
	result, err := transitionLifecycleTx(ctx, tx, lifecycleTransitionRequest{
		LifecycleTransitionParams: LifecycleTransitionParams{
			ResourceType:        domain.LifecycleResourceAgentRun,
			ResourceID:          id,
			Status:              p.Status,
			StatusSource:        domain.LifecycleSourceWorker,
			ReasonCode:          reasonCode,
			Reason:              reason,
			RecoveryHint:        recoveryHint,
			ExecutionCheckpoint: checkpoint,
			RelatedJobID:        jobID,
			RelatedRunID:        &id,
		},
		ExpectedStatuses: []string{"starting", "running", "cancelling"},
		AllowNonContract: allowNonContract,
		IgnoreTerminal:   true,
		Fields: []lifecycleFieldUpdate{
			{Column: "exit_code", Args: []any{p.ExitCode}},
			{Column: "session_id", SQL: "nullif(%s,'')", Args: []any{p.SessionID}},
			{Column: "session_mode", SQL: "coalesce(nullif(%s,''),session_mode)", Args: []any{p.SessionMode}},
			{Column: "session_invalidation_reason", SQL: "coalesce(nullif(%s,''),session_invalidation_reason)", Args: []any{p.SessionInvalidationReason}},
			{Column: "termination_reason", SQL: "nullif(%s,'')", Args: []any{p.TerminationReason}},
			{Column: "failure_category", SQL: "nullif(%s,'')", Args: []any{p.FailureCategory}},
			{Column: "duration_ms", Args: []any{p.DurationMS}},
			{Column: "output_bytes", Args: []any{p.OutputBytes}},
			{Column: "output_lines", Args: []any{p.OutputLines}},
			{Column: "event_count", Args: []any{p.EventCount}},
			{Column: "output_truncated", Args: []any{p.OutputTruncated}},
			{Column: "input_tokens", Args: []any{p.InputTokens}},
			{Column: "output_tokens", Args: []any{p.OutputTokens}},
			{Column: "total_tokens", Args: []any{p.TotalTokens}},
			{Column: "cost_amount", SQL: "%s::numeric", Args: []any{costAmount}},
			{Column: "cost_currency", Args: []any{costCurrency}},
			{Column: "finished_at", SQL: "now()"},
		},
	})
	if err != nil {
		return err
	}
	if result.Idempotent {
		return nil
	}
	return tx.Commit(ctx)
}

func mergeAgentRunCheckpoint(raw json.RawMessage, values map[string]any) json.RawMessage {
	checkpoint := map[string]any{}
	_ = json.Unmarshal(raw, &checkpoint)
	for key, value := range values {
		if value != nil {
			checkpoint[key] = value
		}
	}
	merged, err := json.Marshal(checkpoint)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return merged
}

const agentRunColumns = `
	id,project_id,intake_id,plan_id,job_id,task_id,logical_operation_id,
	operation_type,job_attempt,retry_count,provider,command_summary,pid,session_id,
	session_mode,session_invalidation_reason,status,exit_code,queue_wait_ms,duration_ms,
	failure_category,output_bytes,output_lines,event_count,output_truncated,
	input_tokens,output_tokens,total_tokens,cost_amount::text,cost_currency,log_path,
	termination_reason,started_at,finished_at,created_at,updated_at,version`

func scanAgentRun(row pgx.Row) (domain.AgentRun, error) {
	var run domain.AgentRun
	err := row.Scan(
		&run.ID, &run.ProjectID, &run.IntakeID, &run.PlanID, &run.JobID, &run.TaskID,
		&run.LogicalOperationID, &run.OperationType, &run.JobAttempt, &run.RetryCount,
		&run.Provider, &run.CommandSummary, &run.PID, &run.SessionID, &run.SessionMode,
		&run.SessionInvalidationReason, &run.Status, &run.ExitCode, &run.QueueWaitMS,
		&run.DurationMS, &run.FailureCategory, &run.OutputBytes, &run.OutputLines,
		&run.EventCount, &run.OutputTruncated, &run.InputTokens, &run.OutputTokens,
		&run.TotalTokens, &run.CostAmount, &run.CostCurrency, &run.LogPath,
		&run.TerminationReason, &run.StartedAt, &run.FinishedAt, &run.CreatedAt,
		&run.UpdatedAt, &run.Version,
	)
	if err == pgx.ErrNoRows {
		err = domain.ErrNotFound
	}
	if err == nil && run.SessionID != nil {
		reference := observableSessionReference(run.Provider, *run.SessionID)
		run.SessionID = &reference
	}
	return run, err
}

func observableSessionReference(provider, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.HasPrefix(sessionID, "local:") {
		return sessionID
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(provider) + "\x00" + sessionID))
	return "local:" + hex.EncodeToString(digest[:16])
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

// AgentRunObservabilityFilter limits local observability to one project. Time
// boundaries are inclusive and are applied to the physical run start time.
type AgentRunObservabilityFilter struct {
	ProjectID uuid.UUID
	From      *time.Time
	To        *time.Time
	Provider  string
	PlanID    *uuid.UUID
}

type ObservabilityRequirement struct {
	ID     uuid.UUID `json:"id"`
	Title  string    `json:"title,omitempty"`
	Status string    `json:"status"`
}

type ObservabilityPlan struct {
	ID            uuid.UUID `json:"id"`
	RequirementID uuid.UUID `json:"requirementId"`
	Title         string    `json:"title,omitempty"`
	Status        string    `json:"status"`
}

type ObservabilityTask struct {
	ID      uuid.UUID `json:"id"`
	PlanID  uuid.UUID `json:"planId"`
	TaskKey string    `json:"taskKey"`
	Title   string    `json:"title,omitempty"`
	Status  string    `json:"status"`
}

// ObservabilityAgentRun is deliberately an allow-list. In particular it does
// not expose CLI session identifiers, command lines, environment, raw errors,
// source, attachments, or log paths/content.
type ObservabilityAgentRun struct {
	ID                 uuid.UUID  `json:"id"`
	RequirementID      *uuid.UUID `json:"requirementId,omitempty"`
	PlanID             *uuid.UUID `json:"planId,omitempty"`
	TaskID             *uuid.UUID `json:"taskId,omitempty"`
	LogicalOperationID *uuid.UUID `json:"logicalOperationId,omitempty"`
	OperationType      *string    `json:"operationType,omitempty"`
	JobAttempt         *int       `json:"jobAttempt,omitempty"`
	RetryCount         *int       `json:"retryCount,omitempty"`
	Provider           string     `json:"provider"`
	SessionMode        *string    `json:"sessionMode,omitempty"`
	Status             string     `json:"status"`
	ExitCode           *int       `json:"exitCode,omitempty"`
	QueueWaitMS        *int64     `json:"queueWaitMs,omitempty"`
	DurationMS         *int64     `json:"durationMs,omitempty"`
	FailureCategory    *string    `json:"failureCategory,omitempty"`
	OutputBytes        *int64     `json:"outputBytes,omitempty"`
	OutputLines        *int64     `json:"outputLines,omitempty"`
	EventCount         *int64     `json:"eventCount,omitempty"`
	OutputTruncated    *bool      `json:"outputTruncated,omitempty"`
	InputTokens        *int64     `json:"inputTokens,omitempty"`
	OutputTokens       *int64     `json:"outputTokens,omitempty"`
	TotalTokens        *int64     `json:"totalTokens,omitempty"`
	CostAmount         *string    `json:"costAmount,omitempty"`
	CostCurrency       *string    `json:"costCurrency,omitempty"`
	StartedAt          time.Time  `json:"startedAt"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
}

type ObservabilityRate struct {
	Available   bool     `json:"available"`
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
	Value       *float64 `json:"value,omitempty"`
}

type ObservabilityFailureCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type ObservabilityDurationSummary struct {
	Available     bool  `json:"available"`
	CoverageCount int   `json:"coverageCount"`
	TotalMS       int64 `json:"totalMs,omitempty"`
	AverageMS     int64 `json:"averageMs,omitempty"`
}

type ObservabilityDurationTrend struct {
	Bucket      string                       `json:"bucket"`
	RunCount    int                          `json:"runCount"`
	QueueWait   ObservabilityDurationSummary `json:"queueWait"`
	RunDuration ObservabilityDurationSummary `json:"runDuration"`
}

type ObservabilityTokenSummary struct {
	Available     bool   `json:"available"`
	CoverageCount int    `json:"coverageCount"`
	TotalRunCount int    `json:"totalRunCount"`
	InputTokens   *int64 `json:"inputTokens,omitempty"`
	OutputTokens  *int64 `json:"outputTokens,omitempty"`
	TotalTokens   *int64 `json:"totalTokens,omitempty"`
}

type ObservabilityCurrencyCost struct {
	Currency      string `json:"currency"`
	Amount        string `json:"amount"`
	CoverageCount int    `json:"coverageCount"`
}

type ObservabilityCostSummary struct {
	Available     bool                        `json:"available"`
	CoverageCount int                         `json:"coverageCount"`
	TotalRunCount int                         `json:"totalRunCount"`
	Currencies    []ObservabilityCurrencyCost `json:"currencies"`
}

type ObservabilityUsageSummary struct {
	Tokens ObservabilityTokenSummary `json:"tokens"`
	Costs  ObservabilityCostSummary  `json:"costs"`
}

type ObservabilityUsageGroup struct {
	Key   string `json:"key"`
	Title string `json:"title,omitempty"`
	ObservabilityUsageSummary
}

type AgentRunObservabilityAggregates struct {
	SessionReuseRate          ObservabilityRate            `json:"sessionReuseRate"`
	SnapshotRestoreRate       ObservabilityRate            `json:"snapshotRestoreRate"`
	PlanGenerationSuccessRate ObservabilityRate            `json:"planGenerationSuccessRate"`
	TaskExecutionSuccessRate  ObservabilityRate            `json:"taskExecutionSuccessRate"`
	FailureCategories         []ObservabilityFailureCount  `json:"failureCategories"`
	DurationTrend             []ObservabilityDurationTrend `json:"durationTrend"`
	Usage                     struct {
		Overall       ObservabilityUsageSummary `json:"overall"`
		ByProvider    []ObservabilityUsageGroup `json:"byProvider"`
		ByRequirement []ObservabilityUsageGroup `json:"byRequirement"`
		ByPlan        []ObservabilityUsageGroup `json:"byPlan"`
	} `json:"usage"`
}

type AgentRunObservability struct {
	Requirements []ObservabilityRequirement      `json:"requirements"`
	Plans        []ObservabilityPlan             `json:"plans"`
	Tasks        []ObservabilityTask             `json:"tasks"`
	Runs         []ObservabilityAgentRun         `json:"runs"`
	Aggregates   AgentRunObservabilityAggregates `json:"aggregates"`
}

type observabilityRunRecord struct {
	Run               ObservabilityAgentRun
	RequirementTitle  string
	RequirementStatus string
	PlanTitle         string
	PlanStatus        string
	TaskKey           string
	TaskTitle         string
	TaskStatus        string
}

// QueryAgentRunObservability reads only structured local database fields. It
// never reads run logs or invokes a provider/external service.
func (s *Store) QueryAgentRunObservability(ctx context.Context, filter AgentRunObservabilityFilter) (AgentRunObservability, error) {
	args := []any{filter.ProjectID}
	where := []string{"ar.project_id=$1"}
	if filter.From != nil {
		args = append(args, *filter.From)
		where = append(where, fmt.Sprintf("ar.started_at >= $%d", len(args)))
	}
	if filter.To != nil {
		args = append(args, *filter.To)
		where = append(where, fmt.Sprintf("ar.started_at <= $%d", len(args)))
	}
	if filter.Provider != "" {
		args = append(args, filter.Provider)
		where = append(where, fmt.Sprintf("ar.provider = $%d", len(args)))
	}
	if filter.PlanID != nil {
		args = append(args, *filter.PlanID)
		where = append(where, fmt.Sprintf("ar.plan_id = $%d", len(args)))
	}

	rows, err := s.Pool.Query(ctx, `
		SELECT ar.id,coalesce(ar.intake_id,p.intake_id),ar.plan_id,ar.task_id,
		       ar.logical_operation_id,ar.operation_type,ar.job_attempt,ar.retry_count,
		       ar.provider,ar.session_mode,ar.status,ar.exit_code,ar.queue_wait_ms,
		       ar.duration_ms,ar.failure_category,ar.output_bytes,ar.output_lines,
		       ar.event_count,ar.output_truncated,ar.input_tokens,ar.output_tokens,
		       ar.total_tokens,ar.cost_amount::text,ar.cost_currency,ar.started_at,
		       ar.finished_at,coalesce(i.title,''),coalesce(i.status,''),coalesce(p.title,''),coalesce(p.status,''),
		       coalesce(t.task_key,''),coalesce(t.title,''),coalesce(t.status,'')
		FROM agent_runs ar
		LEFT JOIN plans p ON p.id=ar.plan_id
		LEFT JOIN intakes i ON i.id=coalesce(ar.intake_id,p.intake_id)
		LEFT JOIN plan_tasks t ON t.id=ar.task_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY ar.started_at DESC,ar.created_at DESC,ar.id DESC`, args...)
	if err != nil {
		return AgentRunObservability{}, err
	}
	defer rows.Close()

	records := make([]observabilityRunRecord, 0)
	for rows.Next() {
		var record observabilityRunRecord
		r := &record.Run
		if err = rows.Scan(
			&r.ID, &r.RequirementID, &r.PlanID, &r.TaskID, &r.LogicalOperationID,
			&r.OperationType, &r.JobAttempt, &r.RetryCount, &r.Provider, &r.SessionMode,
			&r.Status, &r.ExitCode, &r.QueueWaitMS, &r.DurationMS, &r.FailureCategory,
			&r.OutputBytes, &r.OutputLines, &r.EventCount, &r.OutputTruncated,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens, &r.CostAmount,
			&r.CostCurrency, &r.StartedAt, &r.FinishedAt, &record.RequirementTitle,
			&record.RequirementStatus, &record.PlanTitle, &record.PlanStatus,
			&record.TaskKey, &record.TaskTitle, &record.TaskStatus,
		); err != nil {
			return AgentRunObservability{}, err
		}
		records = append(records, record)
	}
	if err = rows.Err(); err != nil {
		return AgentRunObservability{}, err
	}
	return buildAgentRunObservability(records), nil
}

func buildAgentRunObservability(records []observabilityRunRecord) AgentRunObservability {
	out := AgentRunObservability{
		Requirements: []ObservabilityRequirement{}, Plans: []ObservabilityPlan{},
		Tasks: []ObservabilityTask{}, Runs: []ObservabilityAgentRun{},
	}
	requirements := map[uuid.UUID]ObservabilityRequirement{}
	plans := map[uuid.UUID]ObservabilityPlan{}
	tasks := map[uuid.UUID]ObservabilityTask{}
	for _, record := range records {
		r := record.Run
		out.Runs = append(out.Runs, r)
		if r.RequirementID != nil {
			requirements[*r.RequirementID] = ObservabilityRequirement{ID: *r.RequirementID, Title: record.RequirementTitle, Status: record.RequirementStatus}
		}
		if r.PlanID != nil {
			plan := ObservabilityPlan{ID: *r.PlanID, Title: record.PlanTitle, Status: record.PlanStatus}
			if r.RequirementID != nil {
				plan.RequirementID = *r.RequirementID
			}
			plans[*r.PlanID] = plan
		}
		if r.TaskID != nil && r.PlanID != nil {
			tasks[*r.TaskID] = ObservabilityTask{ID: *r.TaskID, PlanID: *r.PlanID, TaskKey: record.TaskKey, Title: record.TaskTitle, Status: record.TaskStatus}
		}
	}
	for _, item := range requirements {
		out.Requirements = append(out.Requirements, item)
	}
	for _, item := range plans {
		out.Plans = append(out.Plans, item)
	}
	for _, item := range tasks {
		out.Tasks = append(out.Tasks, item)
	}
	sort.Slice(out.Requirements, func(i, j int) bool { return out.Requirements[i].ID.String() < out.Requirements[j].ID.String() })
	sort.Slice(out.Plans, func(i, j int) bool { return out.Plans[i].ID.String() < out.Plans[j].ID.String() })
	sort.Slice(out.Tasks, func(i, j int) bool {
		if out.Tasks[i].PlanID == out.Tasks[j].PlanID {
			return out.Tasks[i].TaskKey < out.Tasks[j].TaskKey
		}
		return out.Tasks[i].PlanID.String() < out.Tasks[j].PlanID.String()
	})
	out.Aggregates = aggregateAgentRunObservability(records)
	return out
}

type logicalOperationAggregate struct {
	operationType string
	status        string
	failure       string
	startedAt     time.Time
}

func aggregateAgentRunObservability(records []observabilityRunRecord) AgentRunObservabilityAggregates {
	var out AgentRunObservabilityAggregates
	out.FailureCategories = []ObservabilityFailureCount{}
	out.DurationTrend = []ObservabilityDurationTrend{}
	out.Usage.ByProvider = []ObservabilityUsageGroup{}
	out.Usage.ByRequirement = []ObservabilityUsageGroup{}
	out.Usage.ByPlan = []ObservabilityUsageGroup{}

	sessionNumerator, snapshotNumerator, sessionDenominator := 0, 0, 0
	operations := map[string]logicalOperationAggregate{}
	trends := map[string]*ObservabilityDurationTrend{}
	usageOverall := newUsageAccumulator("")
	byProvider := map[string]*usageAccumulator{}
	byRequirement := map[string]*usageAccumulator{}
	byPlan := map[string]*usageAccumulator{}

	for _, record := range records {
		r := record.Run
		if r.SessionMode != nil {
			switch *r.SessionMode {
			case domain.AgentRunSessionModeNew, domain.AgentRunSessionModeReused, domain.AgentRunSessionModeSnapshotRestored:
				sessionDenominator++
				if *r.SessionMode == domain.AgentRunSessionModeReused {
					sessionNumerator++
				}
				if *r.SessionMode == domain.AgentRunSessionModeSnapshotRestored {
					snapshotNumerator++
				}
			}
		}
		if r.OperationType != nil {
			key := *r.OperationType + ":run:" + r.ID.String()
			if r.LogicalOperationID != nil {
				key = *r.OperationType + ":logical:" + r.LogicalOperationID.String()
			}
			current, exists := operations[key]
			if !exists || r.StartedAt.After(current.startedAt) {
				failure := ""
				if r.FailureCategory != nil {
					failure = *r.FailureCategory
				}
				operations[key] = logicalOperationAggregate{operationType: *r.OperationType, status: r.Status, failure: failure, startedAt: r.StartedAt}
			}
			if r.Status == "succeeded" {
				current = operations[key]
				current.status = "succeeded"
				current.failure = ""
				operations[key] = current
			}
		}

		bucket := r.StartedAt.UTC().Format("2006-01-02")
		trend := trends[bucket]
		if trend == nil {
			trend = &ObservabilityDurationTrend{Bucket: bucket}
			trends[bucket] = trend
		}
		trend.RunCount++
		addDuration(&trend.QueueWait, r.QueueWaitMS)
		addDuration(&trend.RunDuration, r.DurationMS)

		usageOverall.add(r)
		acc := byProvider[r.Provider]
		if acc == nil {
			acc = newUsageAccumulator(r.Provider)
			byProvider[r.Provider] = acc
		}
		acc.add(r)
		if r.RequirementID != nil {
			key := r.RequirementID.String()
			acc = byRequirement[key]
			if acc == nil {
				acc = newUsageAccumulator(record.RequirementTitle)
				byRequirement[key] = acc
			}
			acc.add(r)
		}
		if r.PlanID != nil {
			key := r.PlanID.String()
			acc = byPlan[key]
			if acc == nil {
				acc = newUsageAccumulator(record.PlanTitle)
				byPlan[key] = acc
			}
			acc.add(r)
		}
	}
	out.SessionReuseRate = makeRate(sessionNumerator, sessionDenominator)
	out.SnapshotRestoreRate = makeRate(snapshotNumerator, sessionDenominator)
	planOK, planDone, taskOK, taskDone := 0, 0, 0, 0
	failures := map[string]int{}
	for _, operation := range operations {
		terminal := isObservabilityTerminal(operation.status)
		switch operation.operationType {
		case domain.AgentRunOperationPlanGeneration:
			if terminal {
				planDone++
				if operation.status == "succeeded" {
					planOK++
				}
			}
		case domain.AgentRunOperationTaskExecution:
			if terminal {
				taskDone++
				if operation.status == "succeeded" {
					taskOK++
				}
			}
		}
		if terminal && operation.status != "succeeded" {
			category := strings.TrimSpace(operation.failure)
			if category == "" {
				category = "unknown"
			}
			failures[category]++
		}
	}
	out.PlanGenerationSuccessRate = makeRate(planOK, planDone)
	out.TaskExecutionSuccessRate = makeRate(taskOK, taskDone)
	for category, count := range failures {
		out.FailureCategories = append(out.FailureCategories, ObservabilityFailureCount{Category: category, Count: count})
	}
	sort.Slice(out.FailureCategories, func(i, j int) bool { return out.FailureCategories[i].Category < out.FailureCategories[j].Category })
	for _, trend := range trends {
		finishDuration(&trend.QueueWait)
		finishDuration(&trend.RunDuration)
		out.DurationTrend = append(out.DurationTrend, *trend)
	}
	sort.Slice(out.DurationTrend, func(i, j int) bool { return out.DurationTrend[i].Bucket < out.DurationTrend[j].Bucket })
	out.Usage.Overall = usageOverall.summary()
	out.Usage.ByProvider = usageGroups(byProvider)
	out.Usage.ByRequirement = usageGroups(byRequirement)
	out.Usage.ByPlan = usageGroups(byPlan)
	return out
}

func isObservabilityTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "timed_out", "interrupted":
		return true
	default:
		return false
	}
}

func makeRate(numerator, denominator int) ObservabilityRate {
	r := ObservabilityRate{Available: denominator > 0, Numerator: numerator, Denominator: denominator}
	if denominator > 0 {
		value := float64(numerator) / float64(denominator)
		r.Value = &value
	}
	return r
}

func addDuration(summary *ObservabilityDurationSummary, value *int64) {
	if value == nil {
		return
	}
	summary.CoverageCount++
	summary.TotalMS += *value
}

func finishDuration(summary *ObservabilityDurationSummary) {
	summary.Available = summary.CoverageCount > 0
	if summary.Available {
		summary.AverageMS = summary.TotalMS / int64(summary.CoverageCount)
	}
}

type usageAccumulator struct {
	title                                  string
	totalRuns, tokenCoverage, costCoverage int
	inputTokens, outputTokens, totalTokens int64
	costs                                  map[string]*big.Rat
	costCounts                             map[string]int
}

func newUsageAccumulator(title string) *usageAccumulator {
	return &usageAccumulator{title: title, costs: map[string]*big.Rat{}, costCounts: map[string]int{}}
}

func (u *usageAccumulator) add(r ObservabilityAgentRun) {
	u.totalRuns++
	if r.InputTokens != nil || r.OutputTokens != nil || r.TotalTokens != nil {
		u.tokenCoverage++
		if r.InputTokens != nil {
			u.inputTokens += *r.InputTokens
		}
		if r.OutputTokens != nil {
			u.outputTokens += *r.OutputTokens
		}
		if r.TotalTokens != nil {
			u.totalTokens += *r.TotalTokens
		}
	}
	if r.CostAmount != nil && r.CostCurrency != nil && strings.TrimSpace(*r.CostCurrency) != "" {
		amount, ok := new(big.Rat).SetString(*r.CostAmount)
		if ok {
			currency := strings.ToUpper(strings.TrimSpace(*r.CostCurrency))
			if u.costs[currency] == nil {
				u.costs[currency] = new(big.Rat)
			}
			u.costs[currency].Add(u.costs[currency], amount)
			u.costCounts[currency]++
			u.costCoverage++
		}
	}
}

func (u *usageAccumulator) summary() ObservabilityUsageSummary {
	result := ObservabilityUsageSummary{
		Tokens: ObservabilityTokenSummary{Available: u.tokenCoverage > 0, CoverageCount: u.tokenCoverage, TotalRunCount: u.totalRuns},
		Costs:  ObservabilityCostSummary{Available: u.costCoverage > 0, CoverageCount: u.costCoverage, TotalRunCount: u.totalRuns, Currencies: []ObservabilityCurrencyCost{}},
	}
	if u.tokenCoverage > 0 {
		input, output, total := u.inputTokens, u.outputTokens, u.totalTokens
		result.Tokens.InputTokens, result.Tokens.OutputTokens, result.Tokens.TotalTokens = &input, &output, &total
	}
	for currency, amount := range u.costs {
		result.Costs.Currencies = append(result.Costs.Currencies, ObservabilityCurrencyCost{Currency: currency, Amount: decimalString(amount), CoverageCount: u.costCounts[currency]})
	}
	sort.Slice(result.Costs.Currencies, func(i, j int) bool { return result.Costs.Currencies[i].Currency < result.Costs.Currencies[j].Currency })
	return result
}

func usageGroups(groups map[string]*usageAccumulator) []ObservabilityUsageGroup {
	out := make([]ObservabilityUsageGroup, 0, len(groups))
	for key, accumulator := range groups {
		out = append(out, ObservabilityUsageGroup{Key: key, Title: accumulator.title, ObservabilityUsageSummary: accumulator.summary()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func decimalString(value *big.Rat) string {
	denominator := new(big.Int).Set(value.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	remainder := new(big.Int)
	twos, fives := 0, 0
	for {
		quotient := new(big.Int)
		quotient.QuoRem(denominator, two, remainder)
		if remainder.Sign() != 0 {
			break
		}
		denominator = quotient
		twos++
	}
	for {
		quotient := new(big.Int)
		quotient.QuoRem(denominator, five, remainder)
		if remainder.Sign() != 0 {
			break
		}
		denominator = quotient
		fives++
	}
	if denominator.Cmp(big.NewInt(1)) != 0 {
		return value.FloatString(18)
	}
	scale := twos
	if fives > scale {
		scale = fives
	}
	numerator := new(big.Int).Set(value.Num())
	if twos < scale {
		numerator.Mul(numerator, new(big.Int).Exp(two, big.NewInt(int64(scale-twos)), nil))
	}
	if fives < scale {
		numerator.Mul(numerator, new(big.Int).Exp(five, big.NewInt(int64(scale-fives)), nil))
	}
	negative := numerator.Sign() < 0
	if negative {
		numerator.Abs(numerator)
	}
	digits := numerator.String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
		digits = strings.TrimRight(digits, "0")
		digits = strings.TrimRight(digits, ".")
	}
	if digits == "" {
		digits = "0"
	}
	if negative && digits != "0" {
		digits = "-" + digits
	}
	return digits
}
