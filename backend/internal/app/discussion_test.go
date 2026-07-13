package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	settings := testProviderSettings()
	adapter, command, args, err := adapterFor(agent.ProviderClaude, settings)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Name() != agent.ProviderClaude || command != settings.ClaudeCommand || !reflect.DeepEqual(args, []string{"--claude-setting"}) {
		t.Fatalf("adapter=%s command=%q args=%v", adapter.Name(), command, args)
	}
	if settings.AgentProvider != agent.ProviderCodex {
		t.Fatalf("explicit override mutated project default: %q", settings.AgentProvider)
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
	inv, provider, summary, err := taskInvocation(settings, agent.ProviderClaude, false, "/workspace", "prompt", "task", "", "/tmp/task.log")
	if err != nil {
		t.Fatal(err)
	}
	if provider != agent.ProviderClaude || inv.Provider != agent.ProviderClaude || inv.Command != settings.ClaudeCommand || summary != settings.ClaudeCommand {
		t.Fatalf("provider=%q invocation=%+v summary=%q", provider, inv, summary)
	}
	if len(inv.Args) < 3 || inv.Args[0] != "--claude-setting" || !strings.Contains(" "+strings.Join(inv.Args, " ")+" ", " -p prompt ") {
		t.Fatalf("args=%v", inv.Args)
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
}
