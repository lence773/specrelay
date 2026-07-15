package httpapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/domain"
	"github.com/lyming99/specrelay/backend/internal/repository"
)

func TestReadAgentRunLogReturnsNewestLinesAndCursor(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "run.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\nfive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, size, hasMore, nextBefore, _, err := readAgentRunLog(dataDir, path, nil, 2)
	if err != nil || strings.Join(lines, ",") != "four,five" || size != 24 || !hasMore || nextBefore == nil {
		t.Fatalf("lines=%q size=%d hasMore=%v nextBefore=%v err=%v", lines, size, hasMore, nextBefore, err)
	}
	lines, _, hasMore, nextBefore, _, err = readAgentRunLog(dataDir, path, nextBefore, 2)
	if err != nil || strings.Join(lines, ",") != "two,three" || !hasMore || nextBefore == nil {
		t.Fatalf("lines=%q hasMore=%v nextBefore=%v err=%v", lines, hasMore, nextBefore, err)
	}
	lines, _, hasMore, nextBefore, _, err = readAgentRunLog(dataDir, path, nextBefore, 2)
	if err != nil || strings.Join(lines, ",") != "one" || hasMore || nextBefore != nil {
		t.Fatalf("lines=%q hasMore=%v nextBefore=%v err=%v", lines, hasMore, nextBefore, err)
	}
}

func TestReadAgentRunLogDoesNotSplitLongEntries(t *testing.T) {
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "run.log")
	long := strings.Repeat("x", 300<<10)
	if err := os.WriteFile(path, []byte("before\n"+long+"\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _, hasMore, _, _, err := readAgentRunLog(dataDir, path, nil, 2)
	if err != nil || strings.Join(lines, ",") != long+",after" || !hasMore {
		t.Fatalf("line sizes=%v hasMore=%v err=%v", []int{len(lines[0]), len(lines[1])}, hasMore, err)
	}
}

func TestReadAgentRunLogRejectsOutsidePath(t *testing.T) {
	_, _, _, _, _, err := readAgentRunLog(t.TempDir(), filepath.Join(t.TempDir(), "outside.log"), nil, 50)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err=%v", err)
	}
}

func TestReadAgentRunLogAcceptsHistoricalSpecRelayLogDirectories(t *testing.T) {
	configHome := filepath.Join(t.TempDir(), "config")
	dataHome := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	cases := []struct {
		name string
		path string
	}{
		{
			name: "legacy host backend",
			path: filepath.Join(configHome, "specrelay-production", "logs", "legacy.log"),
		},
		{
			name: "packaged desktop app",
			path: filepath.Join(dataHome, desktopAppIdentifier, "data", "logs", "desktop.log"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tc.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path, []byte("historical run\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			lines, _, _, _, _, err := readAgentRunLog(filepath.Join(t.TempDir(), "current-data"), tc.path, nil, 50)
			if err != nil || strings.Join(lines, ",") != "historical run" {
				t.Fatalf("lines=%q err=%v", lines, err)
			}
		})
	}
}

func TestReadAgentRunLogAcceptsPackagedDesktopDefaultUserDataLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	path := filepath.Join(home, ".local", "share", desktopAppIdentifier, "data", "logs", "desktop.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("desktop run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _, _, _, _, err := readAgentRunLog(filepath.Join(t.TempDir(), "current-data"), path, nil, 50)
	if err != nil || strings.Join(lines, ",") != "desktop run" {
		t.Fatalf("lines=%q err=%v", lines, err)
	}
}

func TestParseObservabilityRequestRejectsInvalidFilters(t *testing.T) {
	projectID := uuid.New()
	cases := []struct {
		name  string
		query string
		code  string
	}{
		{name: "time format", query: "from=yesterday", code: "invalid_time"},
		{name: "time range", query: "from=2026-07-15T02%3A00%3A00Z&to=2026-07-15T01%3A00%3A00Z", code: "invalid_time_range"},
		{name: "provider", query: "provider=remote", code: "invalid_provider"},
		{name: "plan", query: "planId=not-a-uuid", code: "invalid_plan"},
		{name: "page", query: "page=0", code: "invalid_page"},
		{name: "page size", query: "pageSize=201", code: "invalid_page_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+projectID.String()+"/observability?"+tc.query, nil)
			response := httptest.NewRecorder()
			if _, ok := parseObservabilityRequest(response, request, projectID); ok {
				t.Fatal("invalid filter was accepted")
			}
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body apiError
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.code {
				t.Fatalf("code=%q want=%q", body.Code, tc.code)
			}
		})
	}
}

func TestObservabilityExportRejectsInvalidFormatAndOptions(t *testing.T) {
	projectID := uuid.New()
	cases := []struct {
		name  string
		query string
		code  string
	}{
		{name: "format", query: "format=xml", code: "invalid_export_format"},
		{name: "format casing", query: "format=JSON", code: "invalid_export_format"},
		{name: "metadata option", query: "includeProjectName=maybe", code: "invalid_export_option"},
		{name: "boolean shorthand", query: "includeWorkspacePath=1", code: "invalid_export_option"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+projectID.String()+"/observability/export?"+tc.query, nil)
			request.SetPathValue("id", projectID.String())
			response := httptest.NewRecorder()
			new(Server).exportAgentRunObservability(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body apiError
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tc.code {
				t.Fatalf("code=%q want=%q", body.Code, tc.code)
			}
		})
	}
}

func TestObservabilityExportDefaultsRedactSensitiveMetadata(t *testing.T) {
	requirementID, planID, taskID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	operationType, sessionMode := "task_execution", "reused"
	result := repository.AgentRunObservability{
		Requirements: []repository.ObservabilityRequirement{{ID: requirementID, Title: "secret requirement title"}},
		Plans:        []repository.ObservabilityPlan{{ID: planID, RequirementID: requirementID, Title: "secret plan title"}},
		Tasks:        []repository.ObservabilityTask{{ID: taskID, PlanID: planID, TaskKey: "P003", Title: "secret task title"}},
		Runs: []repository.ObservabilityAgentRun{{
			ID: runID, RequirementID: &requirementID, PlanID: &planID, TaskID: &taskID,
			Provider: "codex", OperationType: &operationType, SessionMode: &sessionMode,
			Status: "succeeded", StartedAt: time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
		}},
	}
	result.Aggregates.Usage.ByRequirement = []repository.ObservabilityUsageGroup{{Key: requirementID.String(), Title: "secret requirement title"}}
	result.Aggregates.Usage.ByPlan = []repository.ObservabilityUsageGroup{{Key: planID.String(), Title: "secret plan title"}}
	redactObservabilityTitles(&result)

	document := observabilityExportDocument{GeneratedAt: time.Now(), ProjectID: uuid.New(), Summary: result}
	jsonBody, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	csvBody, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(jsonBody) + string(csvBody)
	for _, forbidden := range []string{
		"secret requirement title", "secret plan title", "secret task title",
		"projectName", "workspacePath", "sessionId", "commandSummary",
		"terminationReason", "logPath", "environment", "arguments",
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("redacted export contains %q: %s", forbidden, combined)
		}
	}
	if !strings.Contains(combined, runID.String()) || !strings.Contains(combined, "task_execution") || !strings.Contains(combined, "reused") {
		t.Fatalf("safe observability details are missing: %s", combined)
	}
}

func TestObservabilityExportMetadataOptionsAreIndependent(t *testing.T) {
	projectName, workspacePath := "Visible Project", "/visible/workspace"
	document := observabilityExportDocument{
		GeneratedAt: time.Now(), ProjectID: uuid.New(),
		ProjectName: &projectName,
		Summary: repository.AgentRunObservability{
			Requirements: []repository.ObservabilityRequirement{}, Plans: []repository.ObservabilityPlan{},
			Tasks: []repository.ObservabilityTask{}, Runs: []repository.ObservabilityAgentRun{},
		},
	}
	body, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), projectName) || strings.Contains(string(body), workspacePath) {
		t.Fatalf("independent metadata flags were not preserved: %s", body)
	}
}

func TestParseObservabilityRequestAcceptsInclusiveFiltersAndPagination(t *testing.T) {
	projectID, planID := uuid.New(), uuid.New()
	from := "2026-07-15T01:00:00Z"
	to := "2026-07-15T01:00:00Z"
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+projectID.String()+"/observability?from="+from+"&to="+to+"&provider=claude&planId="+planID.String()+"&page=3&pageSize=25", nil)
	response := httptest.NewRecorder()
	parsed, ok := parseObservabilityRequest(response, request, projectID)
	if !ok {
		t.Fatalf("valid request rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if parsed.Filter.From == nil || parsed.Filter.To == nil || !parsed.Filter.From.Equal(*parsed.Filter.To) || parsed.Filter.Provider != "claude" || parsed.Filter.PlanID == nil || *parsed.Filter.PlanID != planID || parsed.Page != 3 || parsed.PageSize != 25 {
		t.Fatalf("parsed request=%+v", parsed)
	}
}

func observabilityExportTestDocument() observabilityExportDocument {
	requirementID, planID, taskID, logicalID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	operation, session, failure := "task_execution", "snapshot_restored", "provider_error"
	retry := 1
	queue, duration, outputBytes, outputLines, events := int64(12), int64(345), int64(678), int64(9), int64(4)
	truncated := true
	input, output, total := int64(10), int64(5), int64(15)
	cost, currency := "0.25", "USD"
	result := repository.AgentRunObservability{
		Requirements: []repository.ObservabilityRequirement{{ID: requirementID, Title: "Requirement title", Status: "open"}},
		Plans:        []repository.ObservabilityPlan{{ID: planID, RequirementID: requirementID, Title: "Plan title", Status: "running"}},
		Tasks:        []repository.ObservabilityTask{{ID: taskID, PlanID: planID, TaskKey: "P005", Title: "Task title", Status: "succeeded"}},
		Runs: []repository.ObservabilityAgentRun{{
			ID: runID, RequirementID: &requirementID, PlanID: &planID, TaskID: &taskID, LogicalOperationID: &logicalID,
			Provider: "claude", OperationType: &operation, SessionMode: &session, RetryCount: &retry,
			QueueWaitMS: &queue, DurationMS: &duration, Status: "failed", FailureCategory: &failure,
			OutputBytes: &outputBytes, OutputLines: &outputLines, EventCount: &events, OutputTruncated: &truncated,
			InputTokens: &input, OutputTokens: &output, TotalTokens: &total, CostAmount: &cost, CostCurrency: &currency,
			StartedAt: time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC),
		}},
	}
	value := 0.5
	result.Aggregates.SessionReuseRate = repository.ObservabilityRate{Available: true, Numerator: 1, Denominator: 2, Value: &value}
	result.Aggregates.SnapshotRestoreRate = repository.ObservabilityRate{Available: true, Numerator: 1, Denominator: 2, Value: &value}
	result.Aggregates.PlanGenerationSuccessRate = repository.ObservabilityRate{Available: false}
	result.Aggregates.TaskExecutionSuccessRate = repository.ObservabilityRate{Available: true, Numerator: 1, Denominator: 2, Value: &value}
	result.Aggregates.FailureCategories = []repository.ObservabilityFailureCount{{Category: failure, Count: 1}}
	result.Aggregates.DurationTrend = []repository.ObservabilityDurationTrend{{
		Bucket: "2026-07-15", RunCount: 1,
		QueueWait:   repository.ObservabilityDurationSummary{Available: true, CoverageCount: 1, TotalMS: queue, AverageMS: queue},
		RunDuration: repository.ObservabilityDurationSummary{Available: true, CoverageCount: 1, TotalMS: duration, AverageMS: duration},
	}}
	usage := repository.ObservabilityUsageSummary{
		Tokens: repository.ObservabilityTokenSummary{Available: true, CoverageCount: 1, TotalRunCount: 1, InputTokens: &input, OutputTokens: &output, TotalTokens: &total},
		Costs:  repository.ObservabilityCostSummary{Available: true, CoverageCount: 1, TotalRunCount: 1, Currencies: []repository.ObservabilityCurrencyCost{{Currency: currency, Amount: cost, CoverageCount: 1}}},
	}
	result.Aggregates.Usage.Overall = usage
	result.Aggregates.Usage.ByProvider = []repository.ObservabilityUsageGroup{{Key: "claude", Title: "claude", ObservabilityUsageSummary: usage}}
	result.Aggregates.Usage.ByRequirement = []repository.ObservabilityUsageGroup{{Key: requirementID.String(), Title: "Requirement title", ObservabilityUsageSummary: usage}}
	result.Aggregates.Usage.ByPlan = []repository.ObservabilityUsageGroup{{Key: planID.String(), Title: "Plan title", ObservabilityUsageSummary: usage}}
	return observabilityExportDocument{
		GeneratedAt: time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC), ProjectID: uuid.New(),
		Filter:  observabilityFilterResponse{Provider: "claude", PlanID: &planID},
		Options: observabilityExportOptions{IncludeBusinessTitles: true}, Summary: result,
	}
}

func parseObservabilityCSVRows(t *testing.T, body []byte) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("CSV has no header")
	}
	header := records[0]
	rows := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		row := make(map[string]string, len(header))
		for index, key := range header {
			row[key] = record[index]
		}
		rows = append(rows, row)
	}
	return rows
}

func findObservabilityCSVRow(t *testing.T, rows []map[string]string, section, key string) map[string]string {
	t.Helper()
	for _, row := range rows {
		if row["section"] == section && (key == "" || row["key"] == key) {
			return row
		}
	}
	t.Fatalf("missing CSV row section=%q key=%q", section, key)
	return nil
}

func TestObservabilityJSONAndCSVContainConsistentStructuredContent(t *testing.T) {
	document := observabilityExportTestDocument()
	jsonBody, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	csvBody, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded observabilityExportDocument
	if err = json.Unmarshal(jsonBody, &decoded); err != nil {
		t.Fatal(err)
	}
	rows := parseObservabilityCSVRows(t, csvBody)
	run := decoded.Summary.Runs[0]
	runRow := findObservabilityCSVRow(t, rows, "run", "")
	if runRow["runId"] != run.ID.String() || runRow["requirementId"] != run.RequirementID.String() || runRow["planId"] != run.PlanID.String() || runRow["taskId"] != run.TaskID.String() ||
		runRow["provider"] != run.Provider || runRow["operationType"] != *run.OperationType || runRow["sessionMode"] != *run.SessionMode || runRow["retryCount"] != "1" ||
		runRow["durationMs"] != "345" || runRow["failureCategory"] != "provider_error" || runRow["totalTokens"] != "15" || runRow["costAmount"] != "0.25" || runRow["costCurrency"] != "USD" {
		t.Fatalf("JSON run=%+v CSV run=%+v", run, runRow)
	}
	rateRow := findObservabilityCSVRow(t, rows, "rate", "taskExecutionSuccessRate")
	if rateRow["numerator"] != "1" || rateRow["denominator"] != "2" || rateRow["value"] != "0.5" {
		t.Fatalf("rate row=%+v", rateRow)
	}
	costRow := findObservabilityCSVRow(t, rows, "usageOverallCostCurrency", "overall")
	if costRow["costAmount"] != "0.25" || costRow["costCurrency"] != "USD" || costRow["coverageCount"] != "1" || costRow["totalCount"] != "1" || costRow["available"] != "true" {
		t.Fatalf("cost row=%+v", costRow)
	}
}

func TestObservabilityEmptyExportKeepsUnavailableSemantics(t *testing.T) {
	document := observabilityExportDocument{
		GeneratedAt: time.Now().UTC(), ProjectID: uuid.New(),
		Summary: repository.AgentRunObservability{
			Requirements: []repository.ObservabilityRequirement{}, Plans: []repository.ObservabilityPlan{},
			Tasks: []repository.ObservabilityTask{}, Runs: []repository.ObservabilityAgentRun{},
		},
	}
	document.Summary.Aggregates.FailureCategories = []repository.ObservabilityFailureCount{}
	document.Summary.Aggregates.DurationTrend = []repository.ObservabilityDurationTrend{}
	document.Summary.Aggregates.Usage.ByProvider = []repository.ObservabilityUsageGroup{}
	document.Summary.Aggregates.Usage.ByRequirement = []repository.ObservabilityUsageGroup{}
	document.Summary.Aggregates.Usage.ByPlan = []repository.ObservabilityUsageGroup{}
	jsonBody, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(jsonBody), `"value":0`) || strings.Contains(string(jsonBody), `"totalTokens":0`) || strings.Contains(string(jsonBody), `"costAmount":"0"`) {
		t.Fatalf("empty JSON fabricated available values: %s", jsonBody)
	}
	csvBody, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	rows := parseObservabilityCSVRows(t, csvBody)
	for _, section := range []string{"usageOverallTokens", "usageOverallCosts"} {
		row := findObservabilityCSVRow(t, rows, section, "overall")
		if row["available"] != "false" || row["coverageCount"] != "0" || row["totalCount"] != "0" || row["totalTokens"] != "" || row["costAmount"] != "" {
			t.Fatalf("empty %s row=%+v", section, row)
		}
	}
}

func TestObservabilityPrivacyDefaultsAndExplicitMetadataSelection(t *testing.T) {
	document := observabilityExportTestDocument()
	projectName := "Secret Project"
	workspacePath := "/Users/alice/private/source"
	document.ProjectName = &projectName
	document.WorkspacePath = &workspacePath
	document.Options = observabilityExportOptions{IncludeProjectName: true, IncludeWorkspacePath: true, IncludeBusinessTitles: true}

	explicitJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	explicitCSV, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{projectName, workspacePath, "Requirement title", "Plan title", "Task title"} {
		if !strings.Contains(string(explicitJSON), selected) || !strings.Contains(string(explicitCSV), selected) {
			t.Fatalf("explicitly selected metadata %q missing", selected)
		}
	}

	document.ProjectName = nil
	document.WorkspacePath = nil
	document.Options = observabilityExportOptions{}
	secretTitle := "source snippet: const apiToken = 'sk-proj-abcdef0123456789'; session-full-0123456789"
	document.Summary.Requirements[0].Title = secretTitle
	document.Summary.Plans[0].Title = secretTitle
	document.Summary.Tasks[0].Title = secretTitle
	document.Summary.Aggregates.Usage.ByRequirement[0].Title = secretTitle
	document.Summary.Aggregates.Usage.ByPlan[0].Title = secretTitle
	redactObservabilityTitles(&document.Summary)
	defaultJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	defaultCSV, err := marshalObservabilityCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(defaultJSON) + string(defaultCSV)
	for _, forbidden := range []string{
		projectName, workspacePath, secretTitle, "session-full-0123456789", "--dangerously-skip-permissions", "BEGIN PRIVATE LOG", "const apiToken", "commandSummary", "sessionId", "terminationReason", "logPath", "arguments", "environment",
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("default export leaked %q: %s", forbidden, combined)
		}
	}
	if regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{12,}|bearer\s+[a-z0-9._-]{12,})`).MatchString(combined) {
		t.Fatalf("default export contains a token-like value: %s", combined)
	}
}

func TestFeedbackCreateInputDecodesFlattenedAssociation(t *testing.T) {
	requirementID := uuid.New()
	planID := uuid.New()
	taskID := uuid.New()
	checkpointID := uuid.New()
	fileID := uuid.New()
	hunkID := uuid.New()
	body := fmt.Sprintf(`{"requirementId":%q,"title":"review","body":"fix it","planId":%q,"taskId":%q,"checkpointId":%q,"fileId":%q,"diffHunkId":%q,"diffLineSide":"new","diffLineStart":7,"diffLineEnd":9}`, requirementID, planID, taskID, checkpointID, fileID, hunkID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/project/feedback", strings.NewReader(body))
	var input feedbackCreateInput
	if err := decodeJSON(req, &input); err != nil {
		t.Fatal(err)
	}
	params := input.feedbackAssociationInput.repositoryParams()
	if input.RequirementID != requirementID || params.PlanID == nil || *params.PlanID != planID || params.TaskID == nil || *params.TaskID != taskID || params.CheckpointID == nil || *params.CheckpointID != checkpointID || params.FileID == nil || *params.FileID != fileID || params.DiffHunkID == nil || *params.DiffHunkID != hunkID || params.DiffLineSide != "new" || params.DiffLineStart == nil || *params.DiffLineStart != 7 || params.DiffLineEnd == nil || *params.DiffLineEnd != 9 {
		t.Fatalf("decoded input=%+v params=%+v", input, params)
	}
}

func TestFeedbackErrorsHaveStableCodesWithoutRelationDetails(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		forbidden string
	}{
		{name: "forbidden", err: fmt.Errorf("secret project title: %w", domain.ErrForbidden), status: http.StatusForbidden, code: "resource_forbidden", forbidden: "secret project title"},
		{name: "hierarchy", err: fmt.Errorf("task belongs to another plan: %w", domain.ErrInvalidFeedbackLink), status: http.StatusBadRequest, code: "invalid_feedback_relation", forbidden: "another plan"},
		{name: "diff", err: fmt.Errorf("line 99 outside private patch: %w", domain.ErrInvalidDiffRange), status: http.StatusBadRequest, code: "invalid_diff_range", forbidden: "private patch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			recorder.Header().Set("X-Request-ID", uuid.NewString())
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			respondStatus(recorder, req, http.StatusOK, nil, test.err)
			if recorder.Code != test.status {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.code || strings.Contains(response.Message, test.forbidden) || strings.Contains(recorder.Body.String(), test.forbidden) {
				t.Fatalf("response=%+v", response)
			}
		})
	}
}
