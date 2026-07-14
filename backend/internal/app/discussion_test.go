package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/agent"
	"github.com/lyming99/specrelay/backend/internal/domain"
)

func TestParseRequirementDiscussion(t *testing.T) {
	result, err := parseRequirementDiscussion([]byte(`{"reply":" 还需要确认兼容范围。 ","title":" 中文 UI ","body":"## 目标\n放大字体","ready":true}`), "旧标题", "旧正文")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reply != "还需要确认兼容范围。" || result.Title != "中文 UI" || result.Body != "## 目标\n放大字体" || !result.Ready {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestParseRequirementDiscussionUsesDraftFallback(t *testing.T) {
	result, err := parseRequirementDiscussion([]byte(`{"reply":"请确认目标平台。","title":"","body":"","ready":false}`), "草稿标题", "草稿正文")
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "草稿标题" || result.Body != "草稿正文" || result.Ready {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestValidateDiscussionInput(t *testing.T) {
	if err := validateDiscussionInput(RequirementDiscussionRequest{Messages: []RequirementDiscussionMessage{{Role: "assistant", Content: "问题"}}}); err == nil {
		t.Fatal("expected last-message validation error")
	}
	if err := validateDiscussionInput(RequirementDiscussionRequest{Messages: []RequirementDiscussionMessage{{Role: "user", Content: "讨论这个需求"}}}); err != nil {
		t.Fatal(err)
	}
}

func testProviderSettings() domain.ProjectSettings {
	return domain.ProjectSettings{
		AgentProvider:     agent.ProviderCodex,
		CodexCommand:      "/project/bin/codex",
		CodexArgs:         json.RawMessage(`["--codex-setting"]`),
		ClaudeCommand:     "/project/bin/claude",
		ClaudeArgs:        json.RawMessage(`["--claude-setting"]`),
		ValidationCommand: "go test ./...",
		Version:           23,
	}
}

func TestAdapterForUsesDefaultProviderCommandAndArgs(t *testing.T) {
	settings := testProviderSettings()
	adapter, command, args, err := adapterFor("", settings)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Name() != agent.ProviderCodex || command != settings.CodexCommand || !reflect.DeepEqual(args, []string{"--codex-setting"}) {
		t.Fatalf("adapter=%s command=%q args=%v", adapter.Name(), command, args)
	}
}

func TestAdapterForExplicitOverrideUsesSelectedProviderSettingsOnly(t *testing.T) {
	for _, test := range []struct {
		requested      string
		projectDefault string
		command        string
		args           []string
	}{
		{requested: agent.ProviderClaude, projectDefault: agent.ProviderCodex, command: "/project/bin/claude", args: []string{"--claude-setting"}},
		{requested: agent.ProviderCodex, projectDefault: agent.ProviderClaude, command: "/project/bin/codex", args: []string{"--codex-setting"}},
	} {
		t.Run(test.requested+"_over_"+test.projectDefault, func(t *testing.T) {
			settings := testProviderSettings()
			settings.AgentProvider = test.projectDefault
			version := settings.Version
			adapter, command, args, err := adapterFor(test.requested, settings)
			if err != nil {
				t.Fatal(err)
			}
			if adapter.Name() != test.requested || command != test.command || !reflect.DeepEqual(args, test.args) {
				t.Fatalf("adapter=%s command=%q args=%v", adapter.Name(), command, args)
			}
			if settings.AgentProvider != test.projectDefault || settings.Version != version {
				t.Fatalf("entry override mutated project settings: provider=%q version=%d", settings.AgentProvider, settings.Version)
			}
		})
	}
}

func TestAdapterForRejectsInvalidProvider(t *testing.T) {
	_, _, _, err := adapterFor("invalid", testProviderSettings())
	if err == nil || !agent.IsInvalidProvider(err) {
		t.Fatalf("err=%v", err)
	}
}

func TestTaskInvocationMapsResolvedProviderCommandAndArgs(t *testing.T) {
	settings := testProviderSettings()
	for _, test := range []struct {
		name      string
		requested string
		provider  string
		command   string
		firstArg  string
	}{
		{name: "project_default", requested: "", provider: agent.ProviderCodex, command: settings.CodexCommand, firstArg: "--codex-setting"},
		{name: "entry_override", requested: agent.ProviderClaude, provider: agent.ProviderClaude, command: settings.ClaudeCommand, firstArg: "--claude-setting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inv, provider, summary, err := taskInvocation(settings, test.requested, false, "/workspace", "prompt", "task", "", "/tmp/task.log")
			if err != nil {
				t.Fatal(err)
			}
			if provider != test.provider || inv.Provider != test.provider || inv.Command != test.command || summary != test.command {
				t.Fatalf("provider=%q invocation=%+v summary=%q", provider, inv, summary)
			}
			if len(inv.Args) < 3 || inv.Args[0] != test.firstArg || !strings.Contains(" "+strings.Join(inv.Args, " ")+" ", " prompt ") {
				t.Fatalf("args=%v", inv.Args)
			}
		})
	}
}

func TestFinalValidationNeverUsesCLIAdapter(t *testing.T) {
	settings := testProviderSettings()
	settings.AgentProvider = "invalid-default"
	settings.CodexCommand = "/must/not/run/codex"
	settings.ClaudeCommand = "/must/not/run/claude"
	inv, provider, summary, err := taskInvocation(settings, "invalid-override", true, "/workspace", "ignored", "task", "session", "/tmp/validation.log")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "validation" || inv.Provider != "validation" || inv.Command != "/bin/sh" || summary != settings.ValidationCommand {
		t.Fatalf("provider=%q invocation=%+v summary=%q", provider, inv, summary)
	}
	if !reflect.DeepEqual(inv.Args, []string{"-lc", settings.ValidationCommand}) {
		t.Fatalf("args=%v", inv.Args)
	}
}

func TestApplicationInvocationsOnlyRegisterStartCallback(t *testing.T) {
	settings := testProviderSettings()
	task, _, _, err := taskInvocation(settings, "", false, "/workspace", "prompt", "task", "", "/tmp/task.log")
	if err != nil {
		t.Fatal(err)
	}
	validation, _, _, err := taskInvocation(settings, "", true, "/workspace", "", "task", "", "/tmp/validation.log")
	if err != nil {
		t.Fatal(err)
	}
	invocations := map[string]agent.Invocation{
		"plan generation":  agent.Codex().GeneratePlan(settings.CodexCommand, nil, "/workspace", "prompt", time.Minute, "/tmp/plan.log"),
		"task execution":   task,
		"final validation": validation,
		"discussion":       agent.Codex().Discuss(settings.CodexCommand, nil, "/workspace", "prompt", time.Minute, "/tmp/discussion.log"),
	}
	service := &Service{}
	for name, inv := range invocations {
		t.Run(name, func(t *testing.T) {
			service.instrumentInvocation(&inv, uuid.New())
			if inv.OnStart == nil {
				t.Fatal("PID callback was not registered")
			}
			if inv.OnOutput != nil {
				t.Fatal("application registered an output callback")
			}
		})
	}
}

func TestJobPayloadProviderOverride(t *testing.T) {
	raw, err := jobPayloadWithProvider(json.RawMessage(`{"taskId":"t"}`), agent.ProviderClaude, true)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskProviderPayload
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Provider != agent.ProviderClaude || !payload.ProviderRequested {
		t.Fatalf("payload=%s", raw)
	}
}

func TestProbeConfiguredAgentsUsesIndependentCommandsAndKeepsPartialResults(t *testing.T) {
	dir := t.TempDir()
	codexCommand := filepath.Join(dir, "codex-probe")
	claudeCommand := filepath.Join(dir, "claude-probe")
	if err := os.WriteFile(codexCommand, []byte("#!/bin/sh\nprintf 'codex:%s:%s' \"$1\" \"$2\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeCommand, []byte("#!/bin/sh\nprintf 'claude:%s:%s' \"$1\" \"$2\"\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := testProviderSettings()
	settings.AgentProvider = "not-a-probe-default"
	originalVersion := settings.Version
	settings.CodexCommand = codexCommand
	settings.CodexArgs = json.RawMessage(`["--codex-probe-setting"]`)
	settings.ClaudeCommand = claudeCommand
	settings.ClaudeArgs = json.RawMessage(`["--claude-probe-setting"]`)

	response := probeConfiguredAgents(context.Background(), settings, dir)
	if len(response.Results) != 2 {
		t.Fatalf("results=%+v", response.Results)
	}
	codexResult, claudeResult := response.Results[0], response.Results[1]
	if codexResult.Provider != agent.ProviderCodex || !codexResult.Available || codexResult.Error != nil || codexResult.ExitCode == nil || *codexResult.ExitCode != 0 {
		t.Fatalf("codex=%+v", codexResult)
	}
	if !strings.Contains(codexResult.Output, "codex:--codex-probe-setting:--version") {
		t.Fatalf("codex output=%q", codexResult.Output)
	}
	if claudeResult.Provider != agent.ProviderClaude || claudeResult.Available || claudeResult.Error == nil || claudeResult.ExitCode == nil || *claudeResult.ExitCode != 7 {
		t.Fatalf("claude=%+v", claudeResult)
	}
	if !strings.Contains(claudeResult.Output, "claude:--claude-probe-setting:--version") {
		t.Fatalf("claude output=%q", claudeResult.Output)
	}
	if settings.AgentProvider != "not-a-probe-default" || settings.Version != originalVersion {
		t.Fatalf("probe mutated project business defaults: provider=%q version=%d", settings.AgentProvider, settings.Version)
	}
}

func TestSessionHelpersPreserveRecentExecutionState(t *testing.T) {
	long := strings.Repeat("中文上下文", 6000)
	truncated := truncateSessionSummary(long)
	if !strings.Contains(truncated, "[上下文快照已截断]") || !utf8.ValidString(truncated) {
		t.Fatalf("unexpected truncation result: %q", truncated[len(truncated)-64:])
	}
	if !isSessionUnavailable(agent.Result{Output: []byte("error: session not found")}, errors.New("CLI failed")) {
		t.Fatal("expected unavailable session to be recognized")
	}
	if isSessionUnavailable(agent.Result{}, errors.New("network unreachable")) {
		t.Fatal("unexpected unavailable-session classification")
	}
}
