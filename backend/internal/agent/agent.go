package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Invocation struct {
	Provider, Command string
	Args              []string
	Dir               string
	Env               []string
	Timeout           time.Duration
	LogPath           string
	MaxLogBytes       int64
	OnStart           func(pid int)
	OnOutput          func(chunk []byte)
}
type Result struct {
	Output              []byte
	ExitCode            int
	SessionID           string
	Duration            time.Duration
	LogPath             string
	TimedOut, Cancelled bool
}
type Adapter interface {
	Name() string
	Probe(context.Context, string, []string, string) (Result, error)
	GeneratePlan(string, []string, string, string, time.Duration, string) Invocation
	Discuss(string, []string, string, string, time.Duration, string) Invocation
	ExecuteTask(string, []string, string, string, string, time.Duration, string) Invocation
	ResumeTask(string, []string, string, string, string, time.Duration, string) Invocation
}
type Runner struct {
	mu        sync.Mutex
	running   map[string]*exec.Cmd
	cancelled map[string]bool
}

func NewRunner() *Runner {
	return &Runner{running: map[string]*exec.Cmd{}, cancelled: map[string]bool{}}
}

var sessionPattern = regexp.MustCompile(`(?i)(?:session(?:_id)?|thread_id)["' :=]+([A-Za-z0-9_-]{6,})`)

var blockedHostAgentEnv = map[string]struct{}{
	"CODEX_THREAD_ID":                {},
	"CODEX_PERMISSION_PROFILE":       {},
	"CODEX_SANDBOX_NETWORK_DISABLED": {},
}

func commandEnv(overrides []string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range append(os.Environ(), overrides...) {
		name, _, _ := strings.Cut(entry, "=")
		if _, blocked := blockedHostAgentEnv[name]; blocked {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (r *Runner) Run(ctx context.Context, key string, inv Invocation) (Result, error) {
	started := time.Now()
	if inv.MaxLogBytes <= 0 {
		inv.MaxLogBytes = 10 << 20
	}
	var cancel context.CancelFunc
	if inv.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, inv.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(inv.LogPath), 0o700); err != nil {
		return Result{}, err
	}
	logFile, err := os.OpenFile(inv.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()
	cmd := exec.Command(inv.Command, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = commandEnv(inv.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Give os/exec a shared writer rather than reading StdoutPipe and StderrPipe
	// ourselves. Cmd.Wait waits for its configured writers to finish; with
	// explicit pipes, calling Wait before both copies complete can close a pipe
	// and lose the tail of fast CLI output (commonly stderr).
	capture := &limitedCapture{limit: inv.MaxLogBytes}
	writer := &streamCapture{log: logFile, capture: capture, onOutput: inv.OnOutput}
	cmd.Stdout = writer
	cmd.Stderr = writer
	r.mu.Lock()
	if _, exists := r.running[key]; exists {
		r.mu.Unlock()
		return Result{}, fmt.Errorf("agent run %q is already active", key)
	}
	// Register before Start so a concurrent stop cannot miss a process in the
	// small window between process creation and registration. CancelPrefix marks
	// the key even when cmd.Process is not populated yet.
	r.running[key] = cmd
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.running, key); delete(r.cancelled, key); r.mu.Unlock() }()
	if err = cmd.Start(); err != nil {
		return Result{}, err
	}
	if inv.OnStart != nil {
		inv.OnStart(cmd.Process.Pid)
	}
	r.mu.Lock()
	cancelledBeforeStart := r.cancelled[key]
	r.mu.Unlock()
	if cancelledBeforeStart && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case waitErr = <-wait:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			waitErr = <-wait
		}
	}
	result := Result{Output: capture.Bytes(), ExitCode: 0, Duration: time.Since(started), LogPath: inv.LogPath}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if match := sessionPattern.FindSubmatch(result.Output); len(match) > 1 {
		result.SessionID = string(match[1])
	}
	result.TimedOut = ctx.Err() == context.DeadlineExceeded
	r.mu.Lock()
	externallyCancelled := r.cancelled[key]
	r.mu.Unlock()
	result.Cancelled = ctx.Err() == context.Canceled || externallyCancelled
	if result.TimedOut {
		return result, context.DeadlineExceeded
	}
	if result.Cancelled {
		return result, context.Canceled
	}
	if waitErr != nil {
		return result, fmt.Errorf("%s exited with code %d: %w", inv.Provider, result.ExitCode, waitErr)
	}
	return result, nil
}
func (r *Runner) Cancel(key string) error {
	r.mu.Lock()
	cmd := r.running[key]
	if cmd != nil {
		r.cancelled[key] = true
	}
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
func (r *Runner) CancelPrefix(prefix string) {
	r.mu.Lock()
	cmds := []*exec.Cmd{}
	for key, cmd := range r.running {
		if strings.HasPrefix(key, prefix) {
			cmds = append(cmds, cmd)
			r.cancelled[key] = true
		}
	}
	r.mu.Unlock()
	for _, cmd := range cmds {
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
	}
}

type limitedCapture struct {
	data  []byte
	limit int64
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	remaining := w.limit - int64(len(w.data))
	if remaining > 0 {
		if int64(len(p)) > remaining {
			w.data = append(w.data, p[:remaining]...)
		} else {
			w.data = append(w.data, p...)
		}
	}
	return len(p), nil
}
func (w *limitedCapture) Bytes() []byte { return append([]byte(nil), w.data...) }

// streamCapture serializes stdout/stderr writes so the bounded in-memory
// capture remains race-free while preserving a single ordered log file.
type streamCapture struct {
	mu       sync.Mutex
	log      io.Writer
	capture  io.Writer
	onOutput func([]byte)
}

func (w *streamCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.log.Write(p); err != nil {
		return 0, err
	}
	if _, err := w.capture.Write(p); err != nil {
		return 0, err
	}
	if w.onOutput != nil {
		w.onOutput(append([]byte(nil), p...))
	}
	return len(p), nil
}

const (
	ProviderCodex  = "codex"
	ProviderClaude = "claude"
)

type InvalidProviderError struct {
	Provider string
}

func (e InvalidProviderError) Error() string {
	return fmt.Sprintf("provider must be %q or %q, got %q", ProviderCodex, ProviderClaude, e.Provider)
}

func IsInvalidProvider(err error) bool {
	var target InvalidProviderError
	return errors.As(err, &target)
}

// ResolveProvider applies an optional request override to the configured
// project default and returns the matching adapter. Command paths and arguments
// deliberately remain outside this function so callers can only obtain them
// from project settings after the provider has been resolved.
func ResolveProvider(requested, fallback string) (Adapter, error) {
	provider := strings.TrimSpace(requested)
	if provider == "" {
		provider = strings.TrimSpace(fallback)
	}
	switch provider {
	case ProviderCodex:
		return Codex(), nil
	case ProviderClaude:
		return Claude(), nil
	default:
		return nil, InvalidProviderError{Provider: provider}
	}
}

type CLIAdapter struct{ provider string }

func Codex() Adapter              { return CLIAdapter{provider: ProviderCodex} }
func Claude() Adapter             { return CLIAdapter{provider: ProviderClaude} }
func (a CLIAdapter) Name() string { return a.provider }
func (a CLIAdapter) Probe(ctx context.Context, command string, args []string, dir string) (Result, error) {
	inv := Invocation{Provider: a.provider, Command: command, Args: append(args, "--version"), Dir: dir, Timeout: 15 * time.Second, LogPath: filepath.Join(os.TempDir(), "specrelay-probe-"+a.provider+".log"), MaxLogBytes: 1 << 20}
	return NewRunner().Run(ctx, "probe", inv)
}
func (a CLIAdapter) GeneratePlan(command string, args []string, workspace, prompt string, timeout time.Duration, logPath string) Invocation {
	args = safeReadOnlyArgs(a.provider, args)
	if a.provider == "codex" {
		args = append(args, "exec", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "--json", prompt)
	} else {
		args = append(args, "-p", prompt, "--output-format", "json", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob", "--no-session-persistence")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}
func (a CLIAdapter) Discuss(command string, args []string, workspace, prompt string, timeout time.Duration, logPath string) Invocation {
	args = safeReadOnlyArgs(a.provider, args)
	if a.provider == "codex" {
		args = append(args, "exec", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "--json", prompt)
	} else {
		args = append(args, "-p", prompt, "--output-format", "json", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob", "--no-session-persistence")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}

func safeReadOnlyArgs(provider string, args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--dangerously-bypass-approvals-and-sandbox" || arg == "--dangerously-bypass-hook-trust" ||
			arg == "--dangerously-skip-permissions" || arg == "--allow-dangerously-skip-permissions" {
			continue
		}
		if provider == "codex" {
			if strings.HasPrefix(arg, "--ask-for-approval=") || strings.HasPrefix(arg, "--sandbox=") {
				continue
			}
			if arg == "--ask-for-approval" || arg == "-a" || arg == "--sandbox" || arg == "-s" {
				i++
				continue
			}
		}
		if provider == "claude" {
			if strings.HasPrefix(arg, "--permission-mode=") || strings.HasPrefix(arg, "--allowedTools=") ||
				strings.HasPrefix(arg, "--allowed-tools=") || strings.HasPrefix(arg, "--disallowedTools=") ||
				strings.HasPrefix(arg, "--disallowed-tools=") {
				continue
			}
			if arg == "--permission-mode" || arg == "--allowedTools" || arg == "--allowed-tools" || arg == "--disallowedTools" || arg == "--disallowed-tools" {
				i++
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}

func (a CLIAdapter) ExecuteTask(command string, args []string, workspace, prompt string, taskID string, timeout time.Duration, logPath string) Invocation {
	if a.provider == "codex" {
		args = append(args, "exec", "--skip-git-repo-check", "--json", prompt)
	} else {
		args = append(args, "-p", prompt, "--output-format", "json")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}
func (a CLIAdapter) ResumeTask(command string, args []string, workspace, prompt, sessionID string, timeout time.Duration, logPath string) Invocation {
	if a.provider == "codex" {
		args = append(args, "exec", "resume", "--skip-git-repo-check", sessionID, prompt)
	} else {
		args = append(args, "--resume", sessionID, "-p", prompt, "--output-format", "json")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}

func ExtractJSON(output []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(output))
	if json.Valid([]byte(trimmed)) {
		if extracted, ok := extractJSONEnvelope([]byte(trimmed)); ok {
			return extracted, nil
		}
		return []byte(trimmed), nil
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !json.Valid([]byte(line)) {
			continue
		}
		if extracted, ok := extractJSONEnvelope([]byte(line)); ok {
			return extracted, nil
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type != "" {
			continue
		}
		return []byte(line), nil
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start && json.Valid([]byte(trimmed[start:end+1])) {
		return []byte(trimmed[start : end+1]), nil
	}
	return nil, fmt.Errorf("agent output does not contain valid JSON")
}

func extractJSONEnvelope(raw []byte) ([]byte, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, false
	}
	for _, key := range []string{"result", "output", "content", "message"} {
		if extracted, ok := extractJSONValue(envelope[key]); ok {
			return extracted, true
		}
	}
	if itemRaw := envelope["item"]; len(itemRaw) > 0 {
		var item map[string]json.RawMessage
		if json.Unmarshal(itemRaw, &item) == nil {
			if extracted, ok := extractJSONValue(item["text"]); ok {
				return extracted, true
			}
		}
	}
	return nil, false
}

func extractJSONValue(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && json.Valid([]byte(text)) {
		return []byte(text), true
	}
	if json.Valid(raw) {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil {
			return raw, true
		}
	}
	return nil, false
}
