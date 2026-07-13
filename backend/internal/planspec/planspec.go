package planspec

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Spec struct {
	Title           string   `json:"title"`
	Summary         string   `json:"summary"`
	Tasks           []Task   `json:"tasks"`
	FinalValidation []string `json:"finalValidation"`
}
type Task struct {
	Title      string   `json:"title"`
	Scope      []string `json:"scope"`
	Acceptance []string `json:"acceptance"`
}
type NumberedTask struct {
	Key      string
	Position int
	Task     Task
}

var drivePath = regexp.MustCompile(`^[A-Za-z]:`)

func Parse(data []byte) (Spec, error) {
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("invalid PlanSpec JSON: %w", err)
	}
	if err := Validate(&spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func Validate(spec *Spec) error {
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Summary = strings.TrimSpace(spec.Summary)
	if spec.Title == "" {
		return errors.New("title is required")
	}
	if spec.Summary == "" {
		return errors.New("summary is required")
	}
	if len(spec.Tasks) == 0 {
		return errors.New("at least one task is required")
	}
	if len(spec.Tasks) > 100 {
		return errors.New("at most 100 tasks are allowed")
	}
	for i := range spec.Tasks {
		t := &spec.Tasks[i]
		t.Title = strings.TrimSpace(t.Title)
		if t.Title == "" {
			return fmt.Errorf("tasks[%d].title is required", i)
		}
		t.Scope = normalizeList(t.Scope, true)
		t.Acceptance = normalizeList(t.Acceptance, false)
		if len(t.Scope) == 0 {
			return fmt.Errorf("tasks[%d].scope is required", i)
		}
		if len(t.Acceptance) == 0 {
			return fmt.Errorf("tasks[%d].acceptance is required", i)
		}
	}
	spec.FinalValidation = normalizeList(spec.FinalValidation, false)
	if len(spec.FinalValidation) == 0 {
		return errors.New("finalValidation is required")
	}
	return nil
}

func normalizeList(values []string, paths bool) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if paths {
			value = strings.ReplaceAll(value, "\\", "/")
			value = strings.TrimPrefix(filepath.Clean(value), "./")
			if value == "." {
				continue
			}
			if strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") || drivePath.MatchString(value) {
				continue
			}
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	if paths {
		sort.Strings(out)
	}
	return out
}

func Tasks(spec Spec) []NumberedTask {
	out := make([]NumberedTask, 0, len(spec.Tasks)+1)
	for i, task := range spec.Tasks {
		out = append(out, NumberedTask{Key: fmt.Sprintf("P%03d", i+1), Position: i + 1, Task: task})
	}
	validation := Task{Title: "Final validation", Scope: []string{"."}, Acceptance: spec.FinalValidation}
	out = append(out, NumberedTask{Key: fmt.Sprintf("P%03d", len(out)+1), Position: len(out) + 1, Task: validation})
	return out
}

func Render(spec Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## Summary\n\n%s\n\n## Tasks\n", spec.Title, spec.Summary)
	for _, item := range Tasks(spec) {
		fmt.Fprintf(&b, "\n### %s · %s\n\n**Scope**\n", item.Key, item.Task.Title)
		for _, scope := range item.Task.Scope {
			fmt.Fprintf(&b, "- `%s`\n", scope)
		}
		b.WriteString("\n**Acceptance**\n")
		for _, acceptance := range item.Task.Acceptance {
			fmt.Fprintf(&b, "- [ ] %s\n", acceptance)
		}
	}
	return b.String()
}
