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
