package agent

import (
	"context"
	"errors"
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

func TestRunnerCancelAllEscalatesForUncooperativeProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group signalling is Unix-specific")
	}
	dir := t.TempDir()
	runner := NewRunner()
	started := make(chan struct{})
	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(done)
		result, runErr = runner.Run(context.Background(), "discussion", Invocation{
			Provider: "fake",
			Command:  "sh",
			// Ignore TERM in the shell so the runner must use its two-second
			// escalation path. This mirrors an HTTP discussion run during app exit.
			Args:    []string{"-c", "trap '' TERM; while :; do sleep 1; done"},
			Dir:     dir,
			LogPath: filepath.Join(dir, "run.log"),
			OnStart: func(int) { close(started) },
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start the CLI")
	}
	runner.CancelAll()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("CancelAll did not terminate the uncooperative CLI process group")
	}
	if !errors.Is(runErr, context.Canceled) || !result.Cancelled {
		t.Fatalf("result=%+v err=%v", result, runErr)
	}
}

func TestRunnerReportsPIDAndLogsOutputWithoutOutputCallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	pid := make(chan int, 1)
	result, err := NewRunner().Run(context.Background(), "callbacks", Invocation{
		Provider: "fake",
		Command:  "sh",
		Args:     []string{"-c", "printf hello; printf error >&2"},
		Dir:      dir,
		Timeout:  time.Second,
		LogPath:  filepath.Join(dir, "run.log"),
		OnStart:  func(value int) { pid <- value },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-pid; got <= 0 {
		t.Fatalf("pid=%d", got)
	}
	if len(result.Output) == 0 {
		t.Fatal("runner did not capture output")
	}
	logged, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != string(result.Output) {
		t.Fatalf("log=%q output=%q", logged, result.Output)
	}
	for _, expected := range []string{"hello", "error"} {
		if !strings.Contains(string(logged), expected) {
			t.Fatalf("log %q does not contain %q", logged, expected)
		}
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
				for _, required := range []string{" exec ", " --sandbox read-only ", " --skip-git-repo-check ", " --json "} {
					if !strings.Contains(joined, required) {
						t.Fatalf("missing %q in Codex plan arguments: %v", required, inv.Args)
					}
				}
			} else {
				for _, required := range []string{" --permission-mode plan ", " --allowedTools Read,Grep,Glob "} {
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
		joined := " " + strings.Join(inv.Args, " ") + " "
		if !strings.Contains(joined, " --skip-git-repo-check ") || !strings.Contains(joined, " --json ") {
			t.Fatalf("%s invocation does not allow a non-Git workspace with JSON output: %v", label, inv.Args)
		}
	}
}

func TestResumePlanAndDiscussionUsePersistentSessions(t *testing.T) {
	for name, adapter := range map[string]Adapter{"codex": Codex(), "claude": Claude()} {
		t.Run(name, func(t *testing.T) {
			for label, inv := range map[string]Invocation{
				"plan":       adapter.ResumePlan(name, nil, "/workspace", "prompt", "session-id", time.Minute, "/tmp/plan.log"),
				"discussion": adapter.ResumeDiscussion(name, nil, "/workspace", "prompt", "session-id", time.Minute, "/tmp/discussion.log"),
			} {
				joined := " " + strings.Join(inv.Args, " ") + " "
				if strings.Contains(joined, " --ephemeral ") || strings.Contains(joined, " --no-session-persistence ") {
					t.Fatalf("%s unexpectedly disables persistence: %v", label, inv.Args)
				}
				if name == "codex" && (!strings.Contains(joined, " exec resume ") || !strings.Contains(joined, " session-id ") || !strings.Contains(joined, " --json ") || !strings.Contains(joined, ` sandbox_mode="read-only" `)) {
					t.Fatalf("invalid or unsafe Codex %s resume arguments: %v", label, inv.Args)
				}
				if name == "claude" && (!strings.Contains(joined, " --resume session-id ") || !strings.Contains(joined, " -p prompt ")) {
					t.Fatalf("invalid Claude %s resume arguments: %v", label, inv.Args)
				}
			}
		})
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
	for _, required := range []string{" exec ", " --sandbox read-only ", " --skip-git-repo-check ", " --json ", " prompt "} {
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
	for _, required := range []string{" -p prompt ", " --output-format json ", " --permission-mode plan ", " --allowedTools Read,Grep,Glob "} {
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
	for _, fallback := range []string{ProviderCodex, ProviderClaude} {
		t.Run(fallback, func(t *testing.T) {
			adapter, err := ResolveProvider("", fallback)
			if err != nil {
				t.Fatal(err)
			}
			if adapter.Name() != fallback {
				t.Fatalf("provider=%q, want fallback %q", adapter.Name(), fallback)
			}
		})
	}
}

func TestResolveProviderPrefersExplicitSelection(t *testing.T) {
	for _, test := range []struct {
		requested string
		fallback  string
	}{
		{requested: ProviderCodex, fallback: ProviderClaude},
		{requested: ProviderClaude, fallback: ProviderCodex},
	} {
		t.Run(test.requested+"_over_"+test.fallback, func(t *testing.T) {
			adapter, err := ResolveProvider(test.requested, test.fallback)
			if err != nil {
				t.Fatal(err)
			}
			if adapter.Name() != test.requested {
				t.Fatalf("provider=%q, want requested %q", adapter.Name(), test.requested)
			}
		})
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
