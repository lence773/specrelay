package planspec

import (
	"strings"
	"testing"
)

func TestParseNormalizeRender(t *testing.T) {
	spec, err := Parse([]byte(`{"title":" Demo ","summary":"Ship it","tasks":[{"title":"API","scope":["./backend/api.go","backend/api.go"],"acceptance":["tests pass"]}],"finalValidation":["all tests pass"]}`))
	if err != nil {
		t.Fatal(err)
	}
	tasks := Tasks(spec)
	if len(tasks) != 2 || tasks[0].Key != "P001" || tasks[1].Key != "P002" {
		t.Fatalf("unexpected tasks %#v", tasks)
	}
	rendered := Render(spec)
	if !strings.Contains(rendered, "### P002 · Final validation") {
		t.Fatal(rendered)
	}
}
func TestRejectEscapingScope(t *testing.T) {
	_, err := Parse([]byte(`{"title":"x","summary":"x","tasks":[{"title":"x","scope":["../secret"],"acceptance":["x"]}],"finalValidation":["x"]}`))
	if err == nil {
		t.Fatal("expected error")
	}
}
