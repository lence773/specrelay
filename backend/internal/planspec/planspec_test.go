package planspec

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseV2NormalizesOrdersAndRendersDeterministically(t *testing.T) {
	spec, err := Parse([]byte(`{
		"version":2,
		"compatibilityMode":false,
		"title":" Graph plan ",
		"summary":"Execute a dependency graph",
		"tasks":[
			{"key":" build_api ","title":"API","dependsOn":["core"],"scope":["./backend/api.go"],"inputs":["core package"],"outputs":["API"],"risks":["migration"],"acceptance":[{"key":"api_done","description":"API tests pass"}],"validationCommands":["go test ./backend/api"]},
			{"key":"core","title":"Core","dependsOn":[],"scope":["backend/core.go"],"inputs":[],"outputs":["core package"],"risks":[],"acceptance":[{"key":"core_done","description":"Core tests pass"}],"validationCommands":["go test ./backend/core"]},
			{"key":"docs","title":"Docs","dependsOn":[],"scope":["docs/plan.md"],"inputs":[],"outputs":["documentation"],"risks":[],"acceptance":[{"key":"docs_done","description":"Documentation is current"}],"validationCommands":[]},
			{"key":"package","title":"Package","dependsOn":["core"],"scope":["backend/package.go"],"inputs":["core package"],"outputs":["package"],"risks":[],"acceptance":[{"key":"package_done","description":"Package tests pass"}],"validationCommands":["go test ./backend/..."]}
		],
		"finalValidation":{"acceptance":[{"key":"final_done","description":"All focused tests pass"}],"commands":["go test ./backend/..."]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.CompatibilityMode {
		t.Fatal("v2 plan unexpectedly entered compatibility mode")
	}
	if spec.Title != "Graph plan" || spec.Tasks[0].Key != "BUILD-API" || spec.Tasks[0].AcceptanceItems[0].Key != "API-DONE" {
		t.Fatalf("normalization failed: %#v", spec)
	}

	order, err := TopologicalOrder(spec)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"CORE", "BUILD-API", "DOCS", "PACKAGE"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %#v, want %#v", order, wantOrder)
	}
	tasks := Tasks(spec)
	if len(tasks) != 5 || tasks[4].Key != "FINAL" || tasks[4].Position != 5 {
		t.Fatalf("unexpected executable tasks: %#v", tasks)
	}
	for i, key := range wantOrder {
		if tasks[i].Key != key || tasks[i].Position != i+1 {
			t.Fatalf("task %d = %#v", i, tasks[i])
		}
	}

	first := Render(spec)
	second := Render(spec)
	if first != second {
		t.Fatal("rendering is not deterministic")
	}
	for _, expected := range []string{
		"> PlanSpec v2 · 结构化模式",
		"### BUILD-API · API",
		"**Dependencies**",
		"**Inputs**",
		"**Outputs**",
		"**Risks**",
		"`API-DONE` API tests pass",
		"**Suggested validation commands**",
		"### FINAL · Final validation",
	} {
		if !strings.Contains(first, expected) {
			t.Fatalf("render missing %q:\n%s", expected, first)
		}
	}
}

func TestLegacyPlanConvertsToExplicitSerialCompatibilityView(t *testing.T) {
	spec, err := Parse([]byte(`{"title":" Demo ","summary":"Ship it","tasks":[{"title":"API","scope":["./backend/api.go","backend/api.go"],"acceptance":["tests pass"]},{"title":"UI","scope":["frontend/ui.tsx"],"acceptance":["UI works"]}],"finalValidation":["all tests pass"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != CurrentVersion || !spec.CompatibilityMode {
		t.Fatalf("legacy plan not marked as compatibility mode: %#v", spec)
	}
	if spec.Tasks[0].Key != "P001" || len(spec.Tasks[0].DependsOn) != 0 {
		t.Fatalf("unexpected first compatibility task: %#v", spec.Tasks[0])
	}
	if spec.Tasks[1].Key != "P002" || !reflect.DeepEqual(spec.Tasks[1].DependsOn, []string{"P001"}) {
		t.Fatalf("legacy tasks were not made serial: %#v", spec.Tasks[1])
	}
	if spec.Tasks[0].AcceptanceItems[0].Key != "P001-A001" || spec.FinalValidationDefinition.Acceptance[0].Key != "FINAL-A001" {
		t.Fatalf("legacy acceptance was not structured: %#v %#v", spec.Tasks[0].AcceptanceItems, spec.FinalValidationDefinition)
	}
	tasks := Tasks(spec)
	if len(tasks) != 3 || tasks[2].Key != "P003" || tasks[2].Task.Title != "Final validation" {
		t.Fatalf("legacy executable view changed: %#v", tasks)
	}
	if rendered := Render(spec); !strings.Contains(rendered, "兼容模式") || !strings.Contains(rendered, "### P003 · Final validation") {
		t.Fatal(rendered)
	}

	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"version":2`, `"compatibilityMode":true`, `"key":"P001-A001"`, `"finalValidation":{"acceptance"`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("canonical compatibility JSON missing %s: %s", expected, encoded)
		}
	}
	if _, err = Parse(encoded); err != nil {
		t.Fatalf("canonical compatibility view must parse: %v", err)
	}
	var decoded Spec
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("standard JSON unmarshaling must remain supported: %v", err)
	}
	if err = Validate(&decoded); err != nil || !decoded.CompatibilityMode {
		t.Fatalf("unmarshaled compatibility view is invalid: %#v, %v", decoded, err)
	}
}

func TestValidationReportsGraphAndKeyProblems(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		codes []string
	}{
		{
			name: "normalized duplicate task keys",
			json: v2PlanJSON(
				`{"key":"api_task","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]},` +
					`{"key":" api-task ","title":"B","dependsOn":[],"scope":["b"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"B-1","description":"done"}],"validationCommands":[]}`,
			),
			codes: []string{"task.key.duplicate"},
		},
		{
			name:  "normalized duplicate acceptance keys",
			json:  v2PlanJSON(`{"key":"A","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"check_one","description":"one"},{"key":" check-one ","description":"two"}],"validationCommands":[]}`),
			codes: []string{"acceptance.key.duplicate"},
		},
		{
			name:  "dangling dependency",
			json:  v2PlanJSON(`{"key":"A","title":"A","dependsOn":["missing"],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]}`),
			codes: []string{"dependency.dangling"},
		},
		{
			name:  "self dependency",
			json:  v2PlanJSON(`{"key":"A","title":"A","dependsOn":["a"],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]}`),
			codes: []string{"dependency.self"},
		},
		{
			name: "cycle",
			json: v2PlanJSON(
				`{"key":"A","title":"A","dependsOn":["B"],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]},` +
					`{"key":"B","title":"B","dependsOn":["A"],"scope":["b"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"B-1","description":"done"}],"validationCommands":[]}`,
			),
			codes: []string{"dependency.cycle"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := parseProblems(t, tt.json)
			for _, code := range tt.codes {
				if !hasProblemCode(problems, code) {
					t.Fatalf("missing problem %q in %#v", code, problems)
				}
			}
			for _, problem := range problems {
				if problem.Code == "" || problem.Path == "" || problem.Message == "" {
					t.Fatalf("problem is not UI-ready: %#v", problem)
				}
			}
		})
	}
}

func TestValidationReportsAcceptanceAndFinalValidationProblems(t *testing.T) {
	incomplete := v2PlanJSON(`{"key":"A","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"","description":""}],"validationCommands":[]}`)
	if problems := parseProblems(t, incomplete); !hasProblemCode(problems, "acceptance.incomplete") {
		t.Fatalf("missing incomplete acceptance problem: %#v", problems)
	}

	missingAcceptance := v2PlanJSON(`{"key":"A","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[],"validationCommands":[]}`)
	if problems := parseProblems(t, missingAcceptance); !hasProblemCode(problems, "acceptance.required") {
		t.Fatalf("missing acceptance-required problem: %#v", problems)
	}

	missingFinal := `{"version":2,"compatibilityMode":false,"title":"x","summary":"x","tasks":[{"key":"A","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]}]}`
	if problems := parseProblems(t, missingFinal); !hasProblemCode(problems, "finalValidation.required") {
		t.Fatalf("missing final-validation problem: %#v", problems)
	}

	noCommands := strings.Replace(v2PlanJSON(`{"key":"A","title":"A","dependsOn":[],"scope":["a"],"inputs":[],"outputs":[],"risks":[],"acceptance":[{"key":"A-1","description":"done"}],"validationCommands":[]}`), `"commands":["go test ./..."]`, `"commands":[]`, 1)
	if problems := parseProblems(t, noCommands); !hasProblemCode(problems, "finalValidation.commands.required") {
		t.Fatalf("missing final commands problem: %#v", problems)
	}
}

func TestRejectEscapingScopeWithExplicitProblem(t *testing.T) {
	problems := parseProblems(t, `{"title":"x","summary":"x","tasks":[{"title":"x","scope":["../secret"],"acceptance":["x"]}],"finalValidation":["x"]}`)
	if !hasProblemCode(problems, "task.scope.invalid") || !hasProblemCode(problems, "task.scope.required") {
		t.Fatalf("unexpected problems: %#v", problems)
	}
}

func TestParseAllowsWorkspaceRootScope(t *testing.T) {
	spec, err := Parse([]byte(v2PlanJSON(`{"key":"P001","title":"Bootstrap","dependsOn":[],"scope":["."],"inputs":["empty workspace"],"outputs":["project skeleton"],"risks":["initial structure"],"acceptance":[{"key":"P001-A001","description":"workspace root is covered"}],"validationCommands":["go test ./..."]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Tasks) != 1 || len(spec.Tasks[0].Scope) != 1 || spec.Tasks[0].Scope[0] != "." {
		t.Fatalf("scope=%#v", spec.Tasks[0].Scope)
	}
}

func TestLegacyCompositeLiteralRemainsSupported(t *testing.T) {
	spec := Spec{
		Title:           "Legacy",
		Summary:         "Source compatibility",
		Tasks:           []Task{{Title: "Implement", Scope: []string{"backend"}, Acceptance: []string{"passes"}}},
		FinalValidation: []string{"tests pass"},
	}
	if err := Validate(&spec); err != nil {
		t.Fatal(err)
	}
	if !spec.CompatibilityMode || spec.Tasks[0].Key != "P001" {
		t.Fatalf("legacy literal was not converted: %#v", spec)
	}
}

func v2PlanJSON(tasks string) string {
	return `{"version":2,"compatibilityMode":false,"title":"x","summary":"x","tasks":[` + tasks + `],"finalValidation":{"acceptance":[{"key":"FINAL-1","description":"done"}],"commands":["go test ./..."]}}`
}

func parseProblems(t *testing.T, data string) []Problem {
	t.Helper()
	_, err := Parse([]byte(data))
	if err == nil {
		t.Fatal("expected validation error")
	}
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	return validation.Problems
}

func hasProblemCode(problems []Problem, code string) bool {
	for _, problem := range problems {
		if problem.Code == code {
			return true
		}
	}
	return false
}
