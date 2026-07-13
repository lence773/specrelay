package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractJSON(t *testing.T) {
	got, err := ExtractJSON([]byte("noise\n{\"result\":\"{\\\"title\\\":\\\"x\\\"}\"}\n"))
	if err != nil || string(got) != "{\"title\":\"x\"}" {
		t.Fatalf("%s %v", got, err)
	}
}
func TestExtractJSONFromCodexJSONL(t *testing.T) {
	output := []byte("Reading additional input from stdin...\n" +
		`{"type":"thread.started","thread_id":"thread"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"reply\":\"请确认范围\",\"ready\":false}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":12}}` + "\n")
	got, err := ExtractJSON(output)
	if err != nil || string(got) != `{"reply":"请确认范围","ready":false}` {
		t.Fatalf("got=%s err=%v", got, err)
	}
}

func TestRunnerTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	r := NewRunner()
	result, err := r.Run(context.Background(), "x", Invocation{Provider: "fake", Command: "sh", Args: []string{"-c", "sleep 5"}, Dir: dir, Timeout: 50 * time.Millisecond, LogPath: filepath.Join(dir, "run.log")})
	if err == nil || !result.TimedOut {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(result.LogPath); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerWithoutTimeoutCompletesNormally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	result, err := NewRunner().Run(context.Background(), "no-timeout", Invocation{Provider: "fake", Command: "sh", Args: []string{"-c", "sleep 0.05; printf done"}, Dir: dir, Timeout: 0, LogPath: filepath.Join(dir, "run.log")})
	if err != nil || result.TimedOut || result.Cancelled || !strings.Contains(string(result.Output), "done") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunnerWithoutTimeoutStillHonorsParentCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := NewRunner().Run(ctx, "parent-cancel", Invocation{Provider: "fake", Command: "sh", Args: []string{"-c", "sleep 5"}, Dir: dir, Timeout: 0, LogPath: filepath.Join(dir, "run.log")})
	if err == nil || !result.TimedOut {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunnerReportsPIDAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	pid := make(chan int, 1)
	output := make(chan []byte, 4)
	result, err := NewRunner().Run(context.Background(), "callbacks", Invocation{
		Provider: "fake",
		Command:  "sh",
		Args:     []string{"-c", "printf hello; printf error >&2"},
		Dir:      dir,
		Timeout:  time.Second,
		LogPath:  filepath.Join(dir, "run.log"),
		OnStart:  func(value int) { pid <- value },
		OnOutput: func(chunk []byte) { output <- chunk },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-pid; got <= 0 {
		t.Fatalf("pid=%d", got)
	}
	close(output)
	var streamed []byte
	for chunk := range output {
		streamed = append(streamed, chunk...)
	}
	if len(streamed) == 0 || len(result.Output) == 0 {
		t.Fatalf("streamed=%q result=%q", streamed, result.Output)
	}
	logged, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != len(result.Output) {
		t.Fatalf("log bytes=%d output bytes=%d", len(logged), len(result.Output))
	}
}

func TestPlanGenerationUsesReadOnlyCLIMode(t *testing.T) {
	for name, adapter := range map[string]Adapter{"codex": Codex(), "claude": Claude()} {
		t.Run(name, func(t *testing.T) {
			inv := adapter.GeneratePlan(name, []string{"--dangerously-bypass-approvals-and-sandbox", "--dangerously-skip-permissions"}, "/workspace", "plan prompt", time.Minute, "/tmp/plan.log")
			joined := " " + strings.Join(inv.Args, " ") + " "
			if strings.Contains(joined, "dangerously-") {
				t.Fatalf("unsafe plan generation arguments: %v", inv.Args)
			}
			if name == "codex" {
				for _, required := range []string{" exec ", " --sandbox read-only ", " --skip-git-repo-check ", " --ephemeral ", " --json "} {
					if !strings.Contains(joined, required) {
						t.Fatalf("missing %q in Codex plan arguments: %v", required, inv.Args)
					}
				}
			} else {
				for _, required := range []string{" --permission-mode plan ", " --allowedTools Read,Grep,Glob ", " --no-session-persistence "} {
					if !strings.Contains(joined, required) {
						t.Fatalf("missing %q in Claude plan arguments: %v", required, inv.Args)
					}
				}
			}
		})
	}
}

func TestCodexTaskInvocationsAllowNonGitWorkspace(t *testing.T) {
	execute := Codex().ExecuteTask("codex", nil, "/workspace", "prompt", "task", time.Minute, "/tmp/task.log")
	resume := Codex().ResumeTask("codex", nil, "/workspace", "prompt", "session", time.Minute, "/tmp/task.log")
	for label, inv := range map[string]Invocation{"execute": execute, "resume": resume} {
		if !strings.Contains(" "+strings.Join(inv.Args, " ")+" ", " --skip-git-repo-check ") {
			t.Fatalf("%s invocation does not allow a non-Git workspace: %v", label, inv.Args)
		}
	}
}

func TestDiscussionInvocationUsesReadOnlyCodexSandbox(t *testing.T) {
	inv := Codex().Discuss(
		"codex",
		[]string{"--dangerously-bypass-approvals-and-sandbox", "--sandbox", "danger-full-access", "--ask-for-approval=never"},
		"/workspace",
		"prompt",
		time.Minute,
		"/tmp/discussion.log",
	)
	joined := " " + strings.Join(inv.Args, " ") + " "
	for _, forbidden := range []string{"dangerously-bypass", "danger-full-access", "ask-for-approval"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unsafe Codex discussion arguments: %v", inv.Args)
		}
	}
	for _, required := range []string{" exec ", " --sandbox read-only ", " --skip-git-repo-check ", " --ephemeral ", " --json ", " prompt "} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in Codex discussion arguments: %v", required, inv.Args)
		}
	}
}

func TestDiscussionInvocationRestrictsClaudeTools(t *testing.T) {
	inv := Claude().Discuss(
		"claude",
		[]string{"--dangerously-skip-permissions", "--permission-mode=bypassPermissions", "--allowedTools", "Bash,Write"},
		"/workspace",
		"prompt",
		time.Minute,
		"/tmp/discussion.log",
	)
	joined := " " + strings.Join(inv.Args, " ") + " "
	for _, forbidden := range []string{"dangerously-skip", "bypassPermissions", "Bash,Write"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unsafe Claude discussion arguments: %v", inv.Args)
		}
	}
	for _, required := range []string{" -p prompt ", " --output-format json ", " --permission-mode plan ", " --allowedTools Read,Grep,Glob ", " --no-session-persistence "} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %q in Claude discussion arguments: %v", required, inv.Args)
		}
	}
}

func TestCommandEnvDropsHostCodexSessionContext(t *testing.T) {
	blocked := []string{"CODEX_THREAD_ID", "CODEX_PERMISSION_PROFILE", "CODEX_SANDBOX_NETWORK_DISABLED"}
	for _, name := range blocked {
		t.Setenv(name, "host-value")
	}
	env := commandEnv([]string{"SPECRELAY_TEST_ENV=ok", "CODEX_THREAD_ID=override"})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, name := range blocked {
		if strings.Contains(joined, "\n"+name+"=") {
			t.Fatalf("blocked host agent variable leaked: %s", name)
		}
	}
	if !strings.Contains(joined, "\nSPECRELAY_TEST_ENV=ok\n") {
		t.Fatalf("explicit environment override missing: %v", env)
	}
}

func TestResolveProviderUsesConfiguredDefault(t *testing.T) {
	adapter, err := ResolveProvider("", ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Name() != ProviderClaude {
		t.Fatalf("provider=%q", adapter.Name())
	}
}

func TestResolveProviderPrefersExplicitSelection(t *testing.T) {
	adapter, err := ResolveProvider(ProviderCodex, ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Name() != ProviderCodex {
		t.Fatalf("provider=%q", adapter.Name())
	}
}

func TestResolveProviderRejectsUnsupportedValue(t *testing.T) {
	_, err := ResolveProvider("other", ProviderCodex)
	if err == nil || !IsInvalidProvider(err) {
		t.Fatalf("err=%v", err)
	}
	for _, expected := range []string{ProviderCodex, ProviderClaude, "other"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error %q does not mention %q", err, expected)
		}
	}
}
