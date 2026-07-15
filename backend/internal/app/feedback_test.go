package app

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func TestFormatFeedbackPlanningContextIncludesRelationshipAndPriorPlans(t *testing.T) {
	projectID := uuid.New()
	parent := domain.Intake{
		ID:        uuid.New(),
		ProjectID: projectID,
		Kind:      "requirement",
		Title:     "支持本地目录",
		Body:      "项目必须直接访问宿主机目录，并使用本地 CLI。",
	}
	feedback := domain.Intake{
		ID:             uuid.New(),
		ProjectID:      projectID,
		Kind:           "feedback",
		ParentIntakeID: &parent.ID,
		Title:          "增加目录权限提示",
		Body:           "选择目录失败时，需要显示可执行的权限修复建议。",
	}
	plans := []domain.Plan{
		{Title: "本地目录访问计划", Status: "completed", Markdown: "已实现宿主机目录选择与 CLI 调用。"},
		{Title: "可用性优化计划", Status: "ready", Markdown: "待优化目录选择错误提示。"},
	}

	context := formatFeedbackPlanningContext(parent, feedback, plans)

	for _, want := range []string{
		"Planning mode: incremental feedback plan",
		"Create only the smallest safe implementation plan",
		"Title: 支持本地目录",
		"项目必须直接访问宿主机目录",
		"Plan: 本地目录访问计划",
		"Status: completed",
		"已实现宿主机目录选择与 CLI 调用",
		"Feedback to address:",
		"Title: 增加目录权限提示",
		"选择目录失败时，需要显示可执行的权限修复建议",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("planning context missing %q:\n%s", want, context)
		}
	}
}

func TestFormatFeedbackPlanningContextPreservesFeedbackWithinLimit(t *testing.T) {
	parent := domain.Intake{
		ID:        uuid.New(),
		ProjectID: uuid.New(),
		Kind:      "requirement",
		Title:     strings.Repeat("原需求标题", 200),
		Body:      strings.Repeat("原需求上下文", 5000),
	}
	feedback := domain.Intake{
		ID:             uuid.New(),
		ProjectID:      parent.ProjectID,
		Kind:           "feedback",
		ParentIntakeID: &parent.ID,
		Title:          "必须保留的反馈标题",
		Body:           "必须保留的反馈正文：需要增加清晰的权限修复指引。",
	}
	plans := make([]domain.Plan, 4)
	for index := range plans {
		plans[index] = domain.Plan{
			Title:    "历史计划",
			Status:   "completed",
			Markdown: strings.Repeat("历史计划细节", 5000),
		}
	}

	context := formatFeedbackPlanningContext(parent, feedback, plans)
	if got := runeCount(context); got > feedbackPlanContextLimit {
		t.Fatalf("context length=%d, limit=%d", got, feedbackPlanContextLimit)
	}
	if !strings.Contains(context, "必须保留的反馈标题") || !strings.Contains(context, "必须保留的反馈正文") {
		t.Fatalf("feedback was lost after context truncation:\n%s", context)
	}
	if strings.Count(context, "Plan: ") > feedbackPlanCountLimit {
		t.Fatalf("included too many historical plans:\n%s", context)
	}
}

func TestTruncatePlanningTextHonorsRuneLimit(t *testing.T) {
	value := "甲乙丙丁戊己庚辛壬癸"
	got := truncatePlanningText(value, 5)
	if runeCount(got) != 5 {
		t.Fatalf("rune count=%d, value=%q", runeCount(got), got)
	}
}

func TestSummarizeFeedbackTraceBoundsTextAndSelectsDiffLines(t *testing.T) {
	projectID := uuid.New()
	requirementID := uuid.New()
	feedbackID := uuid.New()
	planID := uuid.New()
	taskID := uuid.New()
	checkpointID := uuid.New()
	fileID := uuid.New()
	hunkID := uuid.New()
	start, end := 11, 12
	trace := domain.FeedbackTrace{
		Feedback:    domain.Intake{ID: feedbackID, ProjectID: projectID, Kind: "feedback", Title: strings.Repeat("反馈", 200), Body: strings.Repeat("正文", 7000), Status: "open"},
		Requirement: domain.Intake{ID: requirementID, ProjectID: projectID, Kind: "requirement", Title: strings.Repeat("需求", 200), Body: strings.Repeat("上下文", 5000), Status: "planned"},
		Link:        domain.FeedbackLink{FeedbackID: feedbackID, ProjectID: projectID, RequirementID: requirementID, PlanID: &planID, TaskID: &taskID, CheckpointID: &checkpointID, FileID: &fileID, DiffHunkID: &hunkID, DiffLineSide: "new", DiffLineStart: &start, DiffLineEnd: &end},
		Plan:        &domain.Plan{ID: planID, ProjectID: projectID, IntakeID: requirementID, Title: strings.Repeat("计划", 200), Markdown: strings.Repeat("计划正文", 4000), Status: "completed"},
		Task:        &domain.PlanTask{ID: taskID, ProjectID: projectID, PlanID: planID, TaskKey: "P003", Title: "实现反馈上下文", Status: "succeeded", AcceptanceDefinition: []byte(`{"items":[{"text":"context is bounded"}]}`), AcceptanceStatus: "passed", AcceptanceResult: []byte(`{"status":"passed"}`)},
		Checkpoint:  &domain.PlanExecutionSnapshot{ID: checkpointID, ProjectID: projectID, PlanID: planID, IntakeID: requirementID, TaskID: &taskID, Sequence: 3, Kind: domain.PlanSnapshotKindTaskCheckpoint, ChangeSummary: []byte(`{"summary":"implemented"}`), GitHead: "abc123"},
		File:        &domain.PlanExecutionSnapshotFile{ID: fileID, SnapshotID: checkpointID, Sequence: 1, Path: strings.Repeat("src/", 400) + "file.go", Status: "modified", Additions: 2, Deletions: 1},
		DiffHunk:    &domain.PlanExecutionSnapshotHunk{ID: hunkID, FileID: fileID, Header: "@@ -10,3 +10,4 @@", Patch: "@@ -10,3 +10,4 @@\n same\n-old\n+new\n+extra\n tail\n", OldStartLine: 10, OldLineCount: 3, NewStartLine: 10, NewLineCount: 4},
		Revisions:   []domain.FeedbackRevision{{ID: uuid.New(), FeedbackID: feedbackID, ProjectID: projectID, RequirementID: requirementID, RevisionIntake: domain.Intake{ID: uuid.New(), ProjectID: projectID, Kind: "requirement", Title: strings.Repeat("修订", 200), Status: "planned"}, RevisionPlan: &domain.Plan{ID: uuid.New(), ProjectID: projectID, Status: "running"}}},
	}

	summary := summarizeFeedbackTrace(trace)
	if runeCount(summary.Feedback.Title) > feedbackSummaryTitleLimit || runeCount(summary.Feedback.Body) > feedbackSummaryBodyLimit {
		t.Fatalf("feedback summary exceeded limits: title=%d body=%d", runeCount(summary.Feedback.Title), runeCount(summary.Feedback.Body))
	}
	if summary.Plan == nil || runeCount(summary.Plan.Markdown) > feedbackSummaryPlanLimit {
		t.Fatalf("plan summary missing or unbounded: %+v", summary.Plan)
	}
	if summary.File == nil || runeCount(summary.File.Path) > feedbackSummaryPathLimit {
		t.Fatalf("file summary missing or unbounded: %+v", summary.File)
	}
	if summary.Diff == nil || summary.Diff.Snippet != "+new\n+extra" {
		t.Fatalf("selected diff=%+v", summary.Diff)
	}
	if summary.Task == nil || summary.Task.AcceptanceState != "passed" || !strings.Contains(summary.Task.Acceptance, "context is bounded") {
		t.Fatalf("task acceptance summary=%+v", summary.Task)
	}
	if summary.Revision.CurrentStatus != "running" || len(summary.Revision.Items) != 1 || summary.Revision.Items[0].CurrentStatus != "running" {
		t.Fatalf("revision summary=%+v", summary.Revision)
	}
}

func TestFeedbackReferenceReportsLatestRevisionState(t *testing.T) {
	feedbackID := uuid.New()
	requirementID := uuid.New()
	trace := domain.FeedbackTrace{
		Feedback:    domain.Intake{ID: feedbackID, Title: strings.Repeat("反馈", 200), Status: "open"},
		Requirement: domain.Intake{ID: requirementID},
		Revisions: []domain.FeedbackRevision{
			{RevisionIntake: domain.Intake{Status: "planning"}},
			{RevisionIntake: domain.Intake{Status: "plan_failed"}},
		},
	}

	reference := feedbackReference(trace)
	if reference.ID != feedbackID || reference.RequirementID != requirementID || reference.RevisionStatus != "failed" {
		t.Fatalf("reference=%+v", reference)
	}
	if runeCount(reference.Title) > feedbackSummaryTitleLimit {
		t.Fatalf("title length=%d", runeCount(reference.Title))
	}
}

func TestFormatFeedbackRevisionPlanningContextIncludesPreciseBoundedEvidence(t *testing.T) {
	projectID := uuid.New()
	planID := uuid.New()
	taskID := uuid.New()
	checkpointID := uuid.New()
	fileID := uuid.New()
	hunkID := uuid.New()
	start, end := 41, 42
	trace := domain.FeedbackTrace{
		Feedback:    domain.Intake{ID: uuid.New(), ProjectID: projectID, Kind: "feedback", Title: "失败反馈", Body: "必须优先保留的反馈正文" + strings.Repeat("反馈", 5000)},
		Requirement: domain.Intake{ID: uuid.New(), ProjectID: projectID, Kind: "requirement", Title: "原需求", Body: strings.Repeat("旧上下文", 5000)},
		Link: domain.FeedbackLink{ProjectID: projectID, PlanID: &planID, TaskID: &taskID, CheckpointID: &checkpointID, FileID: &fileID, DiffHunkID: &hunkID,
			DiffLineSide: "new", DiffLineStart: &start, DiffLineEnd: &end},
		Plan: &domain.Plan{ID: planID, Title: "原计划", Markdown: strings.Repeat("原计划内容", 3000)},
		Task: &domain.PlanTask{ID: taskID, PlanID: planID, TaskKey: "P004", Title: "关联任务标题", Status: "failed",
			Scope: []byte(`["backend/internal/app/discussion.go"]`), AcceptanceDefinition: []byte(`{"items":[{"description":"验收标准必须出现"}]}`),
			AcceptanceStatus: domain.AcceptanceStatusFailed, AcceptanceResult: []byte(`{"status":"failed","summary":"任务或验证失败摘要"}`)},
		Checkpoint: &domain.PlanExecutionSnapshot{ID: checkpointID, Sequence: 4, ChangeSummary: []byte(`{"summary":"检查点变更摘要"}`)},
		File:       &domain.PlanExecutionSnapshotFile{ID: fileID, Path: "backend/internal/app/discussion.go"},
		DiffHunk:   &domain.PlanExecutionSnapshotHunk{ID: hunkID, Header: "@@ -40,2 +40,3 @@", Patch: "@@ -40,2 +40,3 @@\n old\n+选中第一行\n+选中第二行\n", OldStartLine: 40, OldLineCount: 2, NewStartLine: 40, NewLineCount: 3},
	}
	revision := domain.Intake{ID: uuid.New(), ProjectID: projectID, Kind: "requirement", Title: "独立修订", Body: "只实现最小增量"}

	context := formatFeedbackRevisionPlanningContext(trace, revision)
	if got := runeCount(context); got > feedbackPlanContextLimit {
		t.Fatalf("context length=%d limit=%d", got, feedbackPlanContextLimit)
	}
	for _, want := range []string{
		"independent incremental feedback revision", "独立修订", "关联任务标题",
		"backend/internal/app/discussion.go", "验收标准必须出现", "任务或验证失败摘要",
		"检查点变更摘要", "+选中第一行", "+选中第二行", "必须优先保留的反馈正文",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("context missing %q:\n%s", want, context)
		}
	}
}
