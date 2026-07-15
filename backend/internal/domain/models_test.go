package domain

import (
	"errors"
	"testing"
)

func TestLifecycleTransitionsByResource(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		from     string
		to       string
		valid    bool
	}{
		{name: "plan completes", resource: "plan", from: "validating", to: "completed", valid: true},
		{name: "plan cannot skip readiness", resource: "plan", from: "generating", to: "running"},
		{name: "task completes", resource: "task", from: "running", to: "succeeded", valid: true},
		{name: "task cannot skip queue", resource: "task", from: "pending", to: "running"},
		{name: "job schedules automatic retry", resource: "job", from: "running", to: "retry_wait", valid: true},
		{name: "retry job can be leased directly", resource: "job", from: "retry_wait", to: "leased", valid: true},
		{name: "job cannot complete before running", resource: "job", from: "leased", to: "succeeded"},
		{name: "agent run starts", resource: "agent_run", from: "starting", to: "running", valid: true},
		{name: "agent run cannot report legacy timeout", resource: "agent_run", from: "running", to: "timed_out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTransition(tt.resource, tt.from, tt.to)
			if tt.valid && err != nil {
				t.Fatalf("ValidateTransition() error = %v", err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("ValidateTransition() error = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestLifecycleSameStateReplayIsKnownStateIdempotent(t *testing.T) {
	disposition, err := EvaluateTransition("task", "running", "running")
	if err != nil || disposition != LifecycleTransitionIdempotent {
		t.Fatalf("known replay = (%q, %v), want idempotent", disposition, err)
	}
	if _, err = EvaluateTransition("task", "unknown", "unknown"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown replay error = %v, want ErrInvalidTransition", err)
	}
}

func TestFinalTerminalStatesCannotBeOverwritten(t *testing.T) {
	tests := []struct {
		resource string
		status   string
		to       string
	}{
		{resource: "plan", status: "completed", to: "running"},
		{resource: "plan", status: "failed", to: "ready"},
		{resource: "task", status: "succeeded", to: "queued"},
		{resource: "task", status: "cancelled", to: "pending"},
		{resource: "job", status: "failed", to: "queued"},
		{resource: "job", status: "cancelled", to: "queued"},
		{resource: "agent_run", status: "succeeded", to: "running"},
		{resource: "agent_run", status: "timed_out", to: "starting"},
	}
	for _, tt := range tests {
		t.Run(tt.resource+"_"+tt.status, func(t *testing.T) {
			if err := ValidateTransition(tt.resource, tt.status, tt.to); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("%s %s -> %s error = %v, want ErrInvalidTransition", tt.resource, tt.status, tt.to, err)
			}
			if err := ValidateTransition(tt.resource, tt.status, tt.status); err != nil {
				t.Fatalf("terminal replay should be idempotent: %v", err)
			}
		})
	}
}

func TestCancellationRequiresCancellingForActiveExecution(t *testing.T) {
	active := []struct {
		resource string
		from     string
	}{
		{resource: "plan", from: "running"},
		{resource: "task", from: "running"},
		{resource: "job", from: "running"},
		{resource: "agent_run", from: "running"},
	}
	for _, tt := range active {
		t.Run(tt.resource, func(t *testing.T) {
			if err := ValidateTransition(tt.resource, tt.from, "cancelled"); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("active direct cancellation error = %v, want ErrInvalidTransition", err)
			}
			if err := ValidateTransition(tt.resource, tt.from, "cancelling"); err != nil {
				t.Fatalf("enter cancelling: %v", err)
			}
			if err := ValidateTransition(tt.resource, "cancelling", "cancelled"); err != nil {
				t.Fatalf("finish cancellation: %v", err)
			}
		})
	}

	for _, transition := range [][3]string{
		{"plan", "ready", "cancelled"},
		{"task", "pending", "cancelled"},
		{"job", "queued", "cancelled"},
	} {
		if err := ValidateTransition(transition[0], transition[1], transition[2]); err != nil {
			t.Fatalf("inactive cancellation %v: %v", transition, err)
		}
	}
}

func TestInterruptedStatesExposeRecoveryEntry(t *testing.T) {
	tests := []struct {
		resource string
		from     string
		resume   string
	}{
		{resource: "plan", from: "running", resume: "running"},
		{resource: "task", from: "running", resume: "queued"},
		{resource: "job", from: "running", resume: "queued"},
		{resource: "agent_run", from: "running", resume: "starting"},
	}
	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			if err := ValidateTransition(tt.resource, tt.from, "interrupted"); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			resource, ok := ParseLifecycleResource(tt.resource)
			if !ok || !IsTerminalStatus(resource, "interrupted") || !IsRecoverableTerminalStatus(resource, "interrupted") {
				t.Fatal("interrupted must be a recoverable terminal state")
			}
			if err := ValidateTransition(tt.resource, "interrupted", tt.resume); err != nil {
				t.Fatalf("resume: %v", err)
			}
		})
	}
}

func TestTypedLifecycleStateClassification(t *testing.T) {
	if !PlanStatusCompleted.IsTerminal() || PlanStatusCompleted.IsRecoverableTerminal() {
		t.Fatal("completed plan must be final and non-recoverable")
	}
	if !TaskStatusInterrupted.IsTerminal() || !TaskStatusInterrupted.IsRecoverableTerminal() {
		t.Fatal("interrupted task must be recoverable terminal")
	}
	if !JobStatusCancelling.IsCancelling() || JobStatusCancelling.IsTerminal() {
		t.Fatal("cancelling job must be in-flight, not terminal")
	}
	if !AgentRunStatusTimedOut.IsTerminal() || AgentRunStatusTimedOut.IsRecoverableTerminal() {
		t.Fatal("historical timeout must remain readable as a non-recoverable terminal")
	}
}

func TestValidateLifecycleMetadata(t *testing.T) {
	valid := LifecycleMetadata{
		StatusSource:        LifecycleSourceRecovery,
		ReasonCode:          LifecycleReasonResumeRequested,
		Reason:              "resume from the last durable checkpoint",
		RecoveryHint:        LifecycleRecoveryResumeFromCheckpoint,
		ExecutionCheckpoint: []byte(`{"step":"validate"}`),
	}
	if err := ValidateLifecycleMetadata(valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.ExecutionCheckpoint = []byte(`[]`)
	if err := ValidateLifecycleMetadata(invalid); !errors.Is(err, ErrInvalidLifecycleMetadata) {
		t.Fatalf("array checkpoint error = %v, want ErrInvalidLifecycleMetadata", err)
	}
}

func TestValidatePlanDriftAuditRequiresReasonChannelAndValidDiff(t *testing.T) {
	if err := ValidatePlanDriftAudit(PlanDriftAuditSnapshotUpdated, "desktop", "accept workspace changes", []byte(`{"head":"changed"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlanDriftAudit(PlanDriftAuditSnapshotUpdated, "desktop", "", []byte(`{}`)); !errors.Is(err, ErrPlanDriftAuditReasonRequired) {
		t.Fatalf("missing reason: got %v", err)
	}
	if err := ValidatePlanDriftAudit("unknown", "desktop", "because", []byte(`{}`)); !errors.Is(err, ErrInvalidPlanDriftAudit) {
		t.Fatalf("unknown action: got %v", err)
	}
	if err := ValidatePlanDriftAudit(PlanDriftAuditExecutionAbandoned, "", "because", []byte(`{}`)); !errors.Is(err, ErrInvalidPlanDriftAudit) {
		t.Fatalf("missing channel: got %v", err)
	}
}
