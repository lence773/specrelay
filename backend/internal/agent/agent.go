package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	HeartbeatInterval time.Duration
	OnStart           func(pid int)
	OnHeartbeat       func()
	OnActivity        func(Activity)
	OnFinish          func()
	OnOutput          func(chunk []byte)
}

type Activity struct {
	OutputBytes int
	Output      bool
	Log         bool
}

type ProcessEvidence struct {
	Running  bool
	Identity string
}
type MetricAvailability string

const (
	MetricAvailable           MetricAvailability = "available"
	MetricFieldsMissing       MetricAvailability = "fields_missing"
	MetricUnknownFormat       MetricAvailability = "unknown_format"
	MetricUnsupportedProvider MetricAvailability = "unsupported_provider"
	MetricOutputTooLarge      MetricAvailability = "output_event_too_large"
)

type Usage struct {
	InputTokens              *int64
	CachedInputTokens        *int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
	OutputTokens             *int64
	TotalTokens              *int64
	CostAmount               string
	CostCurrency             string
	TokenAvailability        MetricAvailability
	CostAvailability         MetricAvailability
}

type OutputSummary struct {
	StepCount         *int64
	CommandsSucceeded *int64
	CommandsFailed    *int64
	PlanTaskCount     *int64
}

type ValidationEvidence struct {
	Command          string
	Status           string
	ExitCode         *int
	StartedAt        *time.Time
	FinishedAt       *time.Time
	Duration         *time.Duration
	OutputSummary    string
	FailureReason    string
	Availability     MetricAvailability
	OutputTruncated  bool
}

const (
	ValidationPassed  = "passed"
	ValidationFailed  = "failed"
	ValidationUnknown = "unknown"
)

type Result struct {
	Output              []byte
	ExitCode            int
	SessionID           string
	Duration            time.Duration
	LogPath             string
	Started             bool
	TimedOut, Cancelled bool
	Interrupted         bool
	OutputBytes         int64
	OutputLines         int64
	EventCount          int64
	OutputTruncated     bool
	EventAvailability   MetricAvailability
	Usage               Usage
	Summary             OutputSummary
	StartedAt           time.Time
	FinishedAt          time.Time
	LogSHA256           string
	FailureReason       string
	ValidationEvidence  []ValidationEvidence
}
type Adapter interface {
	Name() string
	Probe(context.Context, string, []string, string) (Result, error)
	GeneratePlan(string, []string, string, string, time.Duration, string) Invocation
	ResumePlan(string, []string, string, string, string, time.Duration, string) Invocation
	Discuss(string, []string, string, string, time.Duration, string) Invocation
	ResumeDiscussion(string, []string, string, string, string, time.Duration, string) Invocation
	ExecuteTask(string, []string, string, string, string, time.Duration, string) Invocation
	ResumeTask(string, []string, string, string, string, time.Duration, string) Invocation
}
type Runner struct {
	mu        sync.Mutex
	running   map[string]*exec.Cmd
	cancelled map[string]bool
	cancels   map[string]context.CancelFunc
}

func NewRunner() *Runner {
	return &Runner{
		running:   map[string]*exec.Cmd{},
		cancelled: map[string]bool{},
		cancels:   map[string]context.CancelFunc{},
	}
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
	parser := newCLIOutputParser(inv.Provider)
	var cancel context.CancelFunc
	if inv.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, inv.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(inv.LogPath), 0o700); err != nil {
		return Result{Duration: time.Since(started), LogPath: inv.LogPath, Usage: parser.usage(), EventAvailability: parser.eventAvailability()}, err
	}
	logFile, err := os.OpenFile(inv.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{Duration: time.Since(started), LogPath: inv.LogPath, Usage: parser.usage(), EventAvailability: parser.eventAvailability()}, err
	}
	cmd := exec.Command(inv.Command, inv.Args...)
	cmd.Dir = inv.Dir
	cmd.Env = commandEnv(inv.Env)
	configureProcess(cmd)
	// Give os/exec a shared writer rather than reading StdoutPipe and StderrPipe
	// ourselves. Cmd.Wait waits for its configured writers to finish; with
	// explicit pipes, calling Wait before both copies complete can close a pipe
	// and lose the tail of fast CLI output (commonly stderr).
	capture := &limitedCapture{limit: inv.MaxLogBytes}
	writer := &streamCapture{log: logFile, capture: capture, onActivity: inv.OnActivity, onOutput: inv.OnOutput}
	cmd.Stdout = writer
	cmd.Stderr = writer
	r.mu.Lock()
	if _, exists := r.running[key]; exists {
		r.mu.Unlock()
		_ = logFile.Close()
		return Result{}, fmt.Errorf("agent run %q is already active", key)
	}
	// Register before Start so a concurrent stop cannot miss a process in the
	// small window between process creation and registration. CancelPrefix marks
	// the key even when cmd.Process is not populated yet.
	r.running[key] = cmd
	r.cancels[key] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, key)
		delete(r.cancelled, key)
		delete(r.cancels, key)
		r.mu.Unlock()
	}()
	if err = cmd.Start(); err != nil {
		_ = logFile.Close()
		return Result{Duration: time.Since(started), LogPath: inv.LogPath, Usage: parser.usage(), EventAvailability: parser.eventAvailability()}, err
	}
	processStartedAt := time.Now().UTC()
	if inv.OnStart != nil {
		inv.OnStart(cmd.Process.Pid)
	}
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		if inv.OnHeartbeat == nil {
			return
		}
		interval := inv.HeartbeatInterval
		if interval <= 0 {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ticker.C:
				inv.OnHeartbeat()
			}
		}
	}()
	r.mu.Lock()
	cancelledBeforeStart := r.cancelled[key]
	r.mu.Unlock()
	if cancelledBeforeStart && cmd.Process != nil {
		_ = terminateProcessTree(cmd, false)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		_ = terminateProcessTree(cmd, false)
		select {
		case waitErr = <-wait:
		case <-time.After(2 * time.Second):
			_ = terminateProcessTree(cmd, true)
			waitErr = <-wait
		}
	}
	close(heartbeatStop)
	<-heartbeatDone
	processFinishedAt := time.Now().UTC()
	if inv.OnFinish != nil {
		inv.OnFinish()
	}
	closeErr := logFile.Close()
	logSHA256, parseErr := parseCLIOutputLog(inv.LogPath, parser)
	if parseErr != nil {
		parser.readError = parseErr.Error()
	}
	result := Result{
		Output: capture.Bytes(), ExitCode: 0, Duration: processFinishedAt.Sub(processStartedAt), LogPath: inv.LogPath, Started: true,
		OutputBytes: writer.outputBytes, OutputLines: parser.outputLines(), EventCount: parser.eventCount,
		OutputTruncated: capture.Truncated(), EventAvailability: parser.eventAvailability(), Usage: parser.usage(), Summary: parser.summary,
		StartedAt: processStartedAt, FinishedAt: processFinishedAt, LogSHA256: logSHA256,
		FailureReason: parser.failureReason, ValidationEvidence: append([]ValidationEvidence(nil), parser.validations...),
	}
	if closeErr != nil && parseErr == nil {
		parseErr = closeErr
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	result.SessionID = parser.sessionID
	if result.SessionID == "" {
		if match := sessionPattern.FindSubmatch(result.Output); len(match) > 1 {
			result.SessionID = string(match[1])
		}
	}
	result.TimedOut = ctx.Err() == context.DeadlineExceeded
	r.mu.Lock()
	externallyCancelled := r.cancelled[key]
	r.mu.Unlock()
	result.Cancelled = ctx.Err() == context.Canceled || externallyCancelled
	if parseErr != nil && waitErr == nil && !result.TimedOut && !result.Cancelled {
		return result, fmt.Errorf("read completed CLI log for evidence: %w", parseErr)
	}
	if result.TimedOut {
		return result, context.DeadlineExceeded
	}
	if result.Cancelled {
		return result, context.Canceled
	}
	if waitErr != nil {
		result.Interrupted = result.ExitCode < 0
		return result, fmt.Errorf("%s exited with code %d: %w", inv.Provider, result.ExitCode, waitErr)
	}
	return result, nil
}
func (r *Runner) Cancel(key string) error {
	r.mu.Lock()
	cmd := r.running[key]
	cancel := r.cancels[key]
	if cmd != nil {
		r.cancelled[key] = true
	}
	r.mu.Unlock()
	// Cancelling the invocation context is important for non-worker runs (for
	// example a requirement discussion served by an HTTP handler). It lets Run
	// perform its TERM -> bounded wait -> KILL escalation instead of leaving an
	// uncooperative CLI process alive after the server stops accepting requests.
	if cancel != nil {
		cancel()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return terminateProcessTree(cmd, false)
}

// CancelAll terminates every process group started by this backend. It is used
// for process shutdown, including discussion runs which do not have a queue job.
func (r *Runner) CancelAll() {
	r.CancelPrefix("")
}

func (r *Runner) CancelPrefix(prefix string) {
	r.mu.Lock()
	cmds := []*exec.Cmd{}
	cancels := []context.CancelFunc{}
	for key, cmd := range r.running {
		if strings.HasPrefix(key, prefix) {
			cmds = append(cmds, cmd)
			if cancel := r.cancels[key]; cancel != nil {
				cancels = append(cancels, cancel)
			}
			r.cancelled[key] = true
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, cmd := range cmds {
		if cmd != nil && cmd.Process != nil {
			_ = terminateProcessTree(cmd, false)
		}
	}
}

type limitedCapture struct {
	data      []byte
	limit     int64
	truncated bool
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	remaining := w.limit - int64(len(w.data))
	if remaining > 0 {
		if int64(len(p)) > remaining {
			w.data = append(w.data, p[:remaining]...)
			w.truncated = true
		} else {
			w.data = append(w.data, p...)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}
func (w *limitedCapture) Bytes() []byte   { return append([]byte(nil), w.data...) }
func (w *limitedCapture) Truncated() bool { return w.truncated }

// streamCapture serializes stdout/stderr writes so the bounded in-memory
// capture remains race-free while preserving a single ordered log file.
type streamCapture struct {
	mu          sync.Mutex
	log         io.Writer
	capture     io.Writer
	parser      *cliOutputParser
	onActivity  func(Activity)
	onOutput    func([]byte)
	outputBytes int64
}

type parserWriter struct{ parser *cliOutputParser }

func (writer parserWriter) Write(value []byte) (int, error) {
	writer.parser.write(value)
	return len(value), nil
}

func parseCLIOutputLog(path string, parser *cliOutputParser) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	_, err = io.Copy(io.MultiWriter(digest, parserWriter{parser: parser}), file)
	parser.finish()
	return hex.EncodeToString(digest.Sum(nil)), err
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
	w.outputBytes += int64(len(p))
	if w.parser != nil {
		w.parser.write(p)
	}
	if w.onActivity != nil && len(p) > 0 {
		w.onActivity(Activity{OutputBytes: len(p), Output: true, Log: true})
	}
	if w.onOutput != nil {
		w.onOutput(append([]byte(nil), p...))
	}
	return len(p), nil
}

const maxCLIEventBytes = 1 << 20

type cliOutputParser struct {
	provider       string
	line           []byte
	discardingLine bool
	hadOutput      bool
	partialLine    bool
	newlineCount   int64
	eventCount     int64
	recognized     bool
	oversized      bool
	sessionID      string
	usageData      Usage
	summary        OutputSummary
	validations    []ValidationEvidence
	pendingClaude  map[string]ValidationEvidence
	failureReason  string
	readError      string
}

func newCLIOutputParser(provider string) *cliOutputParser {
	return &cliOutputParser{provider: strings.TrimSpace(provider), pendingClaude: map[string]ValidationEvidence{}}
}

func (p *cliOutputParser) write(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	p.hadOutput = true
	for len(chunk) > 0 {
		newline := -1
		for i, value := range chunk {
			if value == '\n' {
				newline = i
				break
			}
		}
		part := chunk
		if newline >= 0 {
			part = chunk[:newline]
		}
		if !p.discardingLine {
			remaining := maxCLIEventBytes - len(p.line)
			if len(part) > remaining {
				p.line = nil
				p.discardingLine = true
				p.oversized = true
			} else {
				p.line = append(p.line, part...)
			}
		}
		if newline < 0 {
			return
		}
		p.newlineCount++
		if !p.discardingLine {
			p.parseLine(p.line)
		}
		p.line = p.line[:0]
		p.discardingLine = false
		chunk = chunk[newline+1:]
	}
}

func (p *cliOutputParser) finish() {
	p.partialLine = len(p.line) > 0 || p.discardingLine
	if !p.discardingLine && len(p.line) > 0 {
		p.parseLine(p.line)
	}
	p.line = nil
}

func (p *cliOutputParser) outputLines() int64 {
	if !p.hadOutput {
		return 0
	}
	if p.partialLine || len(p.line) > 0 || p.discardingLine {
		return p.newlineCount + 1
	}
	return p.newlineCount
}

func (p *cliOutputParser) parseLine(line []byte) {
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 || line[0] != '{' {
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.UseNumber()
	var event map[string]any
	if decoder.Decode(&event) != nil {
		return
	}
	switch p.provider {
	case ProviderCodex:
		p.parseCodexEvent(event)
	case ProviderClaude:
		p.parseClaudeEvent(event)
	}
}

func (p *cliOutputParser) parseCodexEvent(event map[string]any) {
	eventType, _ := event["type"].(string)
	if eventType == "" {
		return
	}
	p.recognized = true
	p.eventCount++
	if p.summary.CommandsSucceeded == nil {
		p.summary.CommandsSucceeded = int64Pointer(0)
		p.summary.CommandsFailed = int64Pointer(0)
	}
	if eventType == "thread.started" {
		p.captureSession(event["thread_id"])
	}
	if eventType == "turn.failed" {
		p.captureFailureReason(event)
	}
	if eventType == "turn.completed" {
		increment(&p.summary.StepCount, 1)
		if usage, ok := event["usage"].(map[string]any); ok {
			addJSONInt(&p.usageData.InputTokens, usage["input_tokens"])
			addJSONInt(&p.usageData.CachedInputTokens, usage["cached_input_tokens"])
			addJSONInt(&p.usageData.OutputTokens, usage["output_tokens"])
			addJSONInt(&p.usageData.TotalTokens, usage["total_tokens"])
			p.captureCost(usage, "total_cost_usd", "USD")
			p.captureCost(usage, "cost_usd", "USD")
		}
	}
	item, _ := event["item"].(map[string]any)
	if item == nil {
		return
	}
	itemType, _ := item["type"].(string)
	if eventType == "item.completed" && itemType == "command_execution" {
		evidence := validationFromCodexItem(item)
		if evidence.Command != "" {
			p.validations = append(p.validations, evidence)
		}
		if evidence.Status == ValidationPassed {
			increment(&p.summary.CommandsSucceeded, 1)
		} else {
			increment(&p.summary.CommandsFailed, 1)
		}
	}
	if itemType == "agent_message" {
		p.capturePlanTaskCount(item["text"])
	}
}

func (p *cliOutputParser) parseClaudeEvent(event map[string]any) {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "assistant":
		p.recognized = true
		p.eventCount++
		p.captureClaudeCommands(event)
	case "user":
		p.recognized = true
		p.eventCount++
		p.captureClaudeResults(event)
	case "result":
		p.recognized = true
		p.eventCount++
		p.captureSession(event["session_id"])
		if turns, ok := jsonInt(event["num_turns"]); ok {
			p.summary.StepCount = int64Pointer(turns)
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			addJSONInt(&p.usageData.InputTokens, usage["input_tokens"])
			addJSONInt(&p.usageData.CacheCreationInputTokens, usage["cache_creation_input_tokens"])
			addJSONInt(&p.usageData.CacheReadInputTokens, usage["cache_read_input_tokens"])
			addJSONInt(&p.usageData.OutputTokens, usage["output_tokens"])
			addJSONInt(&p.usageData.TotalTokens, usage["total_tokens"])
		}
		p.captureCost(event, "total_cost_usd", "USD")
		p.capturePlanTaskCount(event["result"])
		if isError, _ := event["is_error"].(bool); isError {
			p.captureFailureReason(event)
		}
	}
}

func validationFromCodexItem(item map[string]any) ValidationEvidence {
	evidence := ValidationEvidence{
		Command: strings.TrimSpace(jsonText(item["command"])), Status: ValidationUnknown,
		Availability: MetricFieldsMissing,
	}
	if evidence.Command == "" {
		evidence.Command = strings.TrimSpace(jsonText(item["cmd"]))
	}
	if exitCode, ok := jsonSignedInt(item["exit_code"]); ok {
		code := int(exitCode)
		evidence.ExitCode = &code
		evidence.Availability = MetricAvailable
		if code == 0 {
			evidence.Status = ValidationPassed
		} else {
			evidence.Status = ValidationFailed
		}
	} else if status, _ := item["status"].(string); strings.EqualFold(status, "failed") || strings.EqualFold(status, "error") {
		evidence.Status = ValidationFailed
	}
	evidence.StartedAt = jsonTimePointer(item["started_at"])
	evidence.FinishedAt = jsonTimePointer(item["finished_at"])
	evidence.Duration = jsonDurationPointer(item)
	output := firstJSONText(item, "aggregated_output", "output", "stdout", "stderr")
	evidence.OutputSummary, evidence.OutputTruncated = boundEvidenceText(output)
	if evidence.Status == ValidationFailed {
		evidence.FailureReason, _ = boundEvidenceText(firstJSONText(item, "failure_reason", "error", "message"))
		if evidence.FailureReason == "" {
			evidence.FailureReason = evidence.OutputSummary
		}
	}
	return evidence
}

func (p *cliOutputParser) captureClaudeCommands(event map[string]any) {
	for _, content := range claudeContent(event) {
		contentType, _ := content["type"].(string)
		name, _ := content["name"].(string)
		if contentType != "tool_use" || !strings.EqualFold(name, "bash") {
			continue
		}
		input, _ := content["input"].(map[string]any)
		command := strings.TrimSpace(jsonText(input["command"]))
		id := strings.TrimSpace(jsonText(content["id"]))
		if command == "" || id == "" {
			continue
		}
		p.pendingClaude[id] = ValidationEvidence{Command: command, Status: ValidationUnknown, Availability: MetricFieldsMissing}
	}
}

func (p *cliOutputParser) captureClaudeResults(event map[string]any) {
	for _, content := range claudeContent(event) {
		if contentType, _ := content["type"].(string); contentType != "tool_result" {
			continue
		}
		id := strings.TrimSpace(jsonText(content["tool_use_id"]))
		evidence, ok := p.pendingClaude[id]
		if !ok {
			continue
		}
		delete(p.pendingClaude, id)
		output := jsonText(content["content"])
		evidence.OutputSummary, evidence.OutputTruncated = boundEvidenceText(output)
		isError, reported := content["is_error"].(bool)
		if reported {
			if isError {
				evidence.Status = ValidationFailed
				evidence.FailureReason = evidence.OutputSummary
			} else {
				evidence.Status = ValidationPassed
			}
		}
		if exitCode, ok := jsonSignedInt(content["exit_code"]); ok {
			code := int(exitCode)
			evidence.ExitCode = &code
			evidence.Availability = MetricAvailable
			if code == 0 {
				evidence.Status = ValidationPassed
			} else {
				evidence.Status = ValidationFailed
			}
		}
		p.validations = append(p.validations, evidence)
	}
}

func claudeContent(event map[string]any) []map[string]any {
	message, _ := event["message"].(map[string]any)
	items, _ := message["content"].([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if value, ok := item.(map[string]any); ok {
			out = append(out, value)
		}
	}
	return out
}

func (p *cliOutputParser) captureFailureReason(values map[string]any) {
	if p.failureReason != "" {
		return
	}
	p.failureReason, _ = boundEvidenceText(firstJSONText(values, "failure_reason", "error", "message", "result"))
}

func (p *cliOutputParser) captureSession(value any) {
	if p.sessionID != "" {
		return
	}
	if sessionID, ok := value.(string); ok {
		p.sessionID = strings.TrimSpace(sessionID)
	}
}

func (p *cliOutputParser) captureCost(values map[string]any, key, currency string) {
	if p.usageData.CostAmount != "" {
		return
	}
	number, ok := values[key].(json.Number)
	if !ok {
		return
	}
	if _, err := number.Float64(); err != nil || strings.HasPrefix(number.String(), "-") {
		return
	}
	p.usageData.CostAmount = number.String()
	p.usageData.CostCurrency = currency
}

func (p *cliOutputParser) capturePlanTaskCount(value any) {
	text, ok := value.(string)
	if !ok || len(text) > maxCLIEventBytes {
		return
	}
	var object struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if json.Unmarshal([]byte(text), &object) == nil && object.Tasks != nil {
		count := int64(len(object.Tasks))
		p.summary.PlanTaskCount = &count
	}
}

func (p *cliOutputParser) usage() Usage {
	usage := p.usageData
	if usage.InputTokens != nil || usage.CachedInputTokens != nil || usage.CacheCreationInputTokens != nil ||
		usage.CacheReadInputTokens != nil || usage.OutputTokens != nil || usage.TotalTokens != nil {
		usage.TokenAvailability = MetricAvailable
	} else {
		usage.TokenAvailability = p.unavailableReason()
	}
	if usage.CostAmount != "" && usage.CostCurrency != "" {
		usage.CostAvailability = MetricAvailable
	} else {
		usage.CostAvailability = p.unavailableReason()
	}
	return usage
}

func (p *cliOutputParser) eventAvailability() MetricAvailability {
	if p.recognized {
		return MetricAvailable
	}
	return p.unavailableReason()
}

func (p *cliOutputParser) unavailableReason() MetricAvailability {
	if p.readError != "" {
		return MetricFieldsMissing
	}
	if p.provider != ProviderCodex && p.provider != ProviderClaude {
		return MetricUnsupportedProvider
	}
	if p.oversized {
		return MetricOutputTooLarge
	}
	if p.recognized {
		return MetricFieldsMissing
	}
	return MetricUnknownFormat
}

const (
	maxEvidenceSummaryBytes = 16 * 1024
	maxEvidenceSummaryLines = 200
)

func boundEvidenceText(value string) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	if value == "" {
		return "", false
	}
	var builder strings.Builder
	builder.Grow(minInt(len(value), maxEvidenceSummaryBytes))
	lines := 0
	truncated := false
	for _, character := range value {
		if builder.Len()+len(string(character)) > maxEvidenceSummaryBytes || lines >= maxEvidenceSummaryLines {
			truncated = true
			break
		}
		builder.WriteRune(character)
		if character == '\n' {
			lines++
		}
	}
	return builder.String(), truncated
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func jsonText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(jsonText(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	default:
		return ""
	}
}

func firstJSONText(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := strings.TrimSpace(jsonText(values[key])); text != "" {
			return text
		}
	}
	return ""
}

func jsonSignedInt(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil
}

func jsonTimePointer(value any) *time.Time {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func jsonDurationPointer(values map[string]any) *time.Duration {
	for key, unit := range map[string]time.Duration{
		"duration_ms": time.Millisecond, "duration_millis": time.Millisecond,
		"duration_seconds": time.Second,
	} {
		if number, ok := values[key].(json.Number); ok {
			value, err := number.Float64()
			if err == nil && value >= 0 {
				duration := time.Duration(value * float64(unit))
				return &duration
			}
		}
	}
	return nil
}

func jsonInt(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Int64()
	return parsed, err == nil && parsed >= 0
}

func addJSONInt(target **int64, value any) {
	parsed, ok := jsonInt(value)
	if !ok {
		return
	}
	increment(target, parsed)
}

func increment(target **int64, amount int64) {
	if *target == nil {
		*target = int64Pointer(0)
	}
	**target += amount
}

func int64Pointer(value int64) *int64 { return &value }

func commandSucceeded(item map[string]any) bool {
	if exitCode, ok := jsonInt(item["exit_code"]); ok {
		return exitCode == 0
	}
	status, _ := item["status"].(string)
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "success", "succeeded":
		return true
	default:
		return false
	}
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
		// Planning must persist its CLI thread so the approved plan and every
		// later task can continue from the same inspected workspace context.
		args = append(args, "exec", "--sandbox", "read-only", "--skip-git-repo-check", "--json", prompt)
	} else {
		args = append(args, "-p", prompt, "--output-format", "json", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}
func (a CLIAdapter) ResumePlan(command string, args []string, workspace, prompt, sessionID string, timeout time.Duration, logPath string) Invocation {
	args = safeReadOnlyArgs(a.provider, args)
	if a.provider == "codex" {
		args = append(args, "exec", "resume", "-c", `sandbox_mode="read-only"`, "--skip-git-repo-check", "--json", sessionID, prompt)
	} else {
		args = append(args, "--resume", sessionID, "-p", prompt, "--output-format", "json", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}
func (a CLIAdapter) Discuss(command string, args []string, workspace, prompt string, timeout time.Duration, logPath string) Invocation {
	args = safeReadOnlyArgs(a.provider, args)
	if a.provider == "codex" {
		args = append(args, "exec", "--sandbox", "read-only", "--skip-git-repo-check", "--json", prompt)
	} else {
		args = append(args, "-p", prompt, "--output-format", "json", "--permission-mode", "plan", "--allowedTools", "Read,Grep,Glob")
	}
	return Invocation{Provider: a.provider, Command: command, Args: args, Dir: workspace, Timeout: timeout, LogPath: logPath}
}
func (a CLIAdapter) ResumeDiscussion(command string, args []string, workspace, prompt, sessionID string, timeout time.Duration, logPath string) Invocation {
	return a.ResumePlan(command, args, workspace, prompt, sessionID, timeout, logPath)
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
		args = append(args, "exec", "resume", "--skip-git-repo-check", "--json", sessionID, prompt)
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
