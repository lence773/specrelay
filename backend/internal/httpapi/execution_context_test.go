package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/app"
)

func TestRespondMapsExecutionContextDriftToStructuredConflict(t *testing.T) {
	report := app.DriftReport{
		Severity:    app.DriftSeverityNeedsConfirmation,
		Fingerprint: strings.Repeat("a", 64),
		CLIAllowed:  false,
		Differences: []app.ContextDifference{{
			Field:             "provider.execution",
			BaselineValue:     "codex",
			CurrentValue:      "claude",
			Severity:          app.DriftSeverityNeedsConfirmation,
			Reason:            "the task execution provider changed",
			RecommendedAction: app.DriftActionReviewAndAccept,
		}},
	}
	err := &app.DriftBlockedError{PlanID: uuid.New(), Report: report}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plans/invalid/run", nil)
	recorder := httptest.NewRecorder()

	respond(recorder, req, nil, err)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body apiError
	if decodeErr := json.NewDecoder(recorder.Body).Decode(&body); decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if body.Code != "execution_context_drift" {
		t.Fatalf("code = %q, want execution_context_drift", body.Code)
	}
	if body.Message != "Execution context changed and requires review" {
		t.Fatalf("message = %q", body.Message)
	}
	rawDetails, marshalErr := json.Marshal(body.Details)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var got app.DriftReport
	if unmarshalErr := json.Unmarshal(rawDetails, &got); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if got.Severity != report.Severity || got.Fingerprint != report.Fingerprint || got.CLIAllowed {
		t.Fatalf("details = %#v, want %#v", got, report)
	}
	if len(got.Differences) != 1 || got.Differences[0].Field != "provider.execution" {
		t.Fatalf("differences = %#v", got.Differences)
	}
}
