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

func TestCLIOutputParserCollectsCodexUsageAndEventCountsWithoutEstimation(t *testing.T) {
	parser := newCLIOutputParser(ProviderCodex)
	chunks := []string{
		`{"type":"thread.started","thread_id":"thread-actual-123"}` + "\n" +
			`{"type":"item.completed","item":{"type":"command_execution","status":"completed","exit_code":0}}`[:41],
		`{"type":"item.completed","item":{"type":"command_execution","status":"completed","exit_code":0}}`[41:] + "\n" +
			`{"type":"item.completed","item":{"type":"agent_message","text":"{\"tasks\":[{},{}],\"body\":\"must not be retained\"}"}}` + "\n",
		`{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":45,"output_tokens":30}}` + "\n",
	}
	for _, chunk := range chunks {
		parser.write([]byte(chunk))
	}
	parser.finish()
	usage := parser.usage()
	if parser.sessionID != "thread-actual-123" || parser.eventCount != 4 {
		t.Fatalf("session=%q events=%d", parser.sessionID, parser.eventCount)
	}
	if usage.TokenAvailability != MetricAvailable || usage.InputTokens == nil || *usage.InputTokens != 120 ||
		usage.CachedInputTokens == nil || *usage.CachedInputTokens != 45 || usage.OutputTokens == nil || *usage.OutputTokens != 30 {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.TotalTokens != nil {
		t.Fatalf("total tokens were inferred instead of left unavailable: %+v", usage)
	}
	if usage.CostAmount != "" || usage.CostAvailability != MetricFieldsMissing {
		t.Fatalf("missing Codex cost was not marked unavailable: %+v", usage)
	}
	if parser.summary.CommandsSucceeded == nil || *parser.summary.CommandsSucceeded != 1 ||
		parser.summary.CommandsFailed == nil || *parser.summary.CommandsFailed != 0 ||
		parser.summary.StepCount == nil || *parser.summary.StepCount != 1 ||
		parser.summary.PlanTaskCount == nil || *parser.summary.PlanTaskCount != 2 {
		t.Fatalf("summary=%+v", parser.summary)
	}
}

func TestCLIOutputParserCollectsClaudeCacheAndCostFieldsVerbatim(t *testing.T) {
	parser := newCLIOutputParser(ProviderClaude)
	parser.write([]byte(`{"type":"result","subtype":"success","session_id":"claude-session-123","num_turns":3,"total_cost_usd":0.0123400,"usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":40},"result":"{\"tasks\":[{}],\"reply\":\"not retained\"}"}`))
	parser.finish()
	usage := parser.usage()
	if parser.eventAvailability() != MetricAvailable || parser.sessionID != "claude-session-123" {
		t.Fatalf("availability=%s session=%q", parser.eventAvailability(), parser.sessionID)
	}
	if usage.InputTokens == nil || *usage.InputTokens != 10 || usage.CacheCreationInputTokens == nil || *usage.CacheCreationInputTokens != 20 ||
		usage.CacheReadInputTokens == nil || *usage.CacheReadInputTokens != 30 || usage.OutputTokens == nil || *usage.OutputTokens != 40 {
		t.Fatalf("usage=%+v", usage)
	}
	if usage.TotalTokens != nil {
		t.Fatalf("total tokens were inferred: %+v", usage)
	}
	if usage.CostAmount != "0.0123400" || usage.CostCurrency != "USD" || usage.CostAvailability != MetricAvailable {
		t.Fatalf("cost=%+v", usage)
	}
	if parser.summary.StepCount == nil || *parser.summary.StepCount != 3 || parser.summary.PlanTaskCount == nil || *parser.summary.PlanTaskCount != 1 {
		t.Fatalf("summary=%+v", parser.summary)
	}
	if parser.summary.CommandsSucceeded != nil || parser.summary.CommandsFailed != nil {
		t.Fatalf("Claude command counts should be unavailable when the result has no command events: %+v", parser.summary)
	}
}

func TestCLIOutputParserMarksUnknownAndUnsupportedFormatsUnavailable(t *testing.T) {
	known := newCLIOutputParser(ProviderCodex)
	known.write([]byte("plain terminal output\n"))
	known.finish()
	if known.eventAvailability() != MetricUnknownFormat || known.usage().TokenAvailability != MetricUnknownFormat {
		t.Fatalf("known provider availability events=%s usage=%+v", known.eventAvailability(), known.usage())
	}

	unsupported := newCLIOutputParser("other")
	unsupported.write([]byte(`{"type":"result","usage":{"input_tokens":999}}`))
	unsupported.finish()
	if unsupported.eventAvailability() != MetricUnsupportedProvider || unsupported.usage().TokenAvailability != MetricUnsupportedProvider {
		t.Fatalf("unsupported provider availability events=%s usage=%+v", unsupported.eventAvailability(), unsupported.usage())
	}
}

func TestCLIOutputParserBoundsIndividualEvents(t *testing.T) {
	parser := newCLIOutputParser(ProviderCodex)
	parser.write([]byte("{" + strings.Repeat("x", maxCLIEventBytes+128) + "}\n"))
	parser.finish()
	if len(parser.line) != 0 || parser.eventAvailability() != MetricOutputTooLarge {
		t.Fatalf("line=%d availability=%s", len(parser.line), parser.eventAvailability())
	}
}

func TestCLIOutputParserPreservesZeroUsageAndCostAsAvailable(t *testing.T) {
	for name, providerAndOutput := range map[string]struct {
		provider string
		output   string
	}{
		"codex": {
			provider: ProviderCodex,
			output:   `{"type":"turn.completed","usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"total_tokens":0,"total_cost_usd":0}}`,
		},
		"claude": {
			provider: ProviderClaude,
			output:   `{"type":"result","subtype":"success","session_id":"zero-session","num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0,"total_tokens":0},"result":"{}"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			parser := newCLIOutputParser(providerAndOutput.provider)
			parser.write([]byte(providerAndOutput.output))
			parser.finish()
			usage := parser.usage()
			if usage.TokenAvailability != MetricAvailable || usage.CostAvailability != MetricAvailable || usage.CostAmount != "0" || usage.CostCurrency != "USD" {
				t.Fatalf("zero usage was treated as unavailable: %+v", usage)
			}
			for field, value := range map[string]*int64{
				"input": usage.InputTokens, "output": usage.OutputTokens, "total": usage.TotalTokens,
			} {
				if value == nil || *value != 0 {
					t.Fatalf("%s tokens=%v, want an explicit zero", field, value)
				}
			}
		})
	}
}

func TestCLIOutputParserKeepsMissingFieldsUnavailable(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		output   string
	}{
		{name: "codex recognized event without usage", provider: ProviderCodex, output: `{"type":"turn.completed"}`},
		{name: "claude recognized result without usage", provider: ProviderClaude, output: `{"type":"result","subtype":"success","result":"{}"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			parser := newCLIOutputParser(test.provider)
			parser.write([]byte(test.output))
			parser.finish()
			usage := parser.usage()
			if parser.eventAvailability() != MetricAvailable || usage.TokenAvailability != MetricFieldsMissing || usage.CostAvailability != MetricFieldsMissing {
				t.Fatalf("availability events=%s usage=%+v", parser.eventAvailability(), usage)
			}
			if usage.InputTokens != nil || usage.OutputTokens != nil || usage.TotalTokens != nil || usage.CostAmount != "" || usage.CostCurrency != "" {
				t.Fatalf("missing fields were synthesized: %+v", usage)
			}
		})
	}
}

func TestCLIOutputParserAggregatesMultiTurnTokenEventsWithoutInventingTotals(t *testing.T) {
	parser := newCLIOutputParser(ProviderCodex)
	parser.write([]byte(
		`{"type":"thread.started","thread_id":"multi-turn-session"}` + "\n" +
			`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":2,"output_tokens":3,"cost_usd":0.01}}` + "\n" +
			`{"type":"turn.completed","usage":{"input_tokens":20,"cached_input_tokens":4,"output_tokens":6}}` + "\n",
	))
	parser.finish()
	usage := parser.usage()
	if parser.eventCount != 3 || parser.summary.StepCount == nil || *parser.summary.StepCount != 2 {
		t.Fatalf("events=%d summary=%+v", parser.eventCount, parser.summary)
	}
	if usage.InputTokens == nil || *usage.InputTokens != 30 || usage.CachedInputTokens == nil || *usage.CachedInputTokens != 6 || usage.OutputTokens == nil || *usage.OutputTokens != 9 {
		t.Fatalf("multi-turn usage=%+v", usage)
	}
	if usage.TotalTokens != nil {
		t.Fatalf("a missing provider total was inferred: %+v", usage)
	}
	if usage.CostAmount != "0.01" || usage.CostCurrency != "USD" || usage.CostAvailability != MetricAvailable {
		t.Fatalf("reported multi-turn cost was not preserved: %+v", usage)
	}
}

func TestCLIOutputParserLeavesTruncatedAndUnknownUsageUnavailable(t *testing.T) {
	truncated := newCLIOutputParser(ProviderClaude)
	truncated.write([]byte("{" + strings.Repeat("x", maxCLIEventBytes+1) + "}"))
	truncated.finish()
	truncatedUsage := truncated.usage()
	if truncated.eventAvailability() != MetricOutputTooLarge || truncatedUsage.TokenAvailability != MetricOutputTooLarge || truncatedUsage.CostAvailability != MetricOutputTooLarge {
		t.Fatalf("truncated availability events=%s usage=%+v", truncated.eventAvailability(), truncatedUsage)
	}
	if truncatedUsage.InputTokens != nil || truncatedUsage.OutputTokens != nil || truncatedUsage.TotalTokens != nil || truncatedUsage.CostAmount != "" {
		t.Fatalf("truncated output produced usage: %+v", truncatedUsage)
	}

	unknown := newCLIOutputParser(ProviderClaude)
	unknown.write([]byte(`{"type":"future.result.v9","usage":{"input_tokens":999999},"total_cost_usd":999}`))
	unknown.finish()
	unknownUsage := unknown.usage()
	if unknown.eventAvailability() != MetricUnknownFormat || unknownUsage.TokenAvailability != MetricUnknownFormat || unknownUsage.CostAvailability != MetricUnknownFormat {
		t.Fatalf("unknown availability events=%s usage=%+v", unknown.eventAvailability(), unknownUsage)
	}
	if unknownUsage.InputTokens != nil || unknownUsage.OutputTokens != nil || unknownUsage.TotalTokens != nil || unknownUsage.CostAmount != "" || unknownUsage.CostCurrency != "" {
		t.Fatalf("unknown format produced usage: %+v", unknownUsage)
	}
}

func TestRunnerReportsHeartbeatAndOutputActivity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	dir := t.TempDir()
	heartbeats := make(chan struct{}, 8)
	activities := make(chan Activity, 8)
	startedPID := 0
	finished := false
	result, err := NewRunner().Run(context.Background(), "activity", Invocation{
		Provider:          "fake",
		Command:           "sh",
		Args:              []string{"-c", "printf first; sleep 0.08; printf second"},
		Dir:               dir,
		LogPath:           filepath.Join(dir, "activity.log"),
		HeartbeatInterval: 15 * time.Millisecond,
		OnStart: func(pid int) {
			startedPID = pid
		},
		OnHeartbeat: func() {
			heartbeats <- struct{}{}
		},
		OnActivity: func(activity Activity) {
			activities <- activity
		},
		OnFinish: func() {
			finished = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputBytes == 0 {
		t.Fatal("runner reported no output bytes")
	}
	if startedPID <= 0 || !finished {
		t.Fatalf("lifecycle callbacks: pid=%d finished=%t", startedPID, finished)
	}
	if len(heartbeats) == 0 {
		t.Fatal("runner did not report a process heartbeat")
	}
	var observedBytes int
	for len(activities) > 0 {
		activity := <-activities
		if !activity.Output || !activity.Log {
			t.Fatalf("activity=%+v", activity)
		}
		observedBytes += activity.OutputBytes
	}
	if int64(observedBytes) != result.OutputBytes {
		t.Fatalf("activity bytes=%d result bytes=%d", observedBytes, result.OutputBytes)
	}
}

func TestInspectProcessReportsCurrentProcess(t *testing.T) {
	evidence, err := InspectProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.Running {
		t.Fatalf("current process was reported stopped: %+v", evidence)
	}
	if runtime.GOOS == "linux" && evidence.Identity == "" {
		t.Fatalf("current process identity was not reported: %+v", evidence)
	}
}
