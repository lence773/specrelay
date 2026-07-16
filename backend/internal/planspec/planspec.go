package planspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const CurrentVersion = 2

// Spec keeps the legacy string fields so existing callers can construct plans
// while exposing the canonical v2 representation through the structured
// fields. JSON marshaling always emits the canonical versioned representation.
type Spec struct {
	Version                   int                  `json:"-"`
	CompatibilityMode         bool                 `json:"-"`
	Title                     string               `json:"-"`
	Summary                   string               `json:"-"`
	Tasks                     []Task               `json:"-"`
	FinalValidation           []string             `json:"-"`
	FinalValidationDefinition ValidationDefinition `json:"-"`
}

type Task struct {
	Key                string           `json:"-"`
	Title              string           `json:"-"`
	DependsOn          []string         `json:"-"`
	Scope              []string         `json:"-"`
	Inputs             []string         `json:"-"`
	Outputs            []string         `json:"-"`
	Risks              []string         `json:"-"`
	Acceptance         []string         `json:"-"`
	AcceptanceItems    []AcceptanceItem `json:"-"`
	ValidationCommands []string         `json:"-"`
}

type AcceptanceItem struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

type ValidationDefinition struct {
	Acceptance []AcceptanceItem `json:"acceptance"`
	Commands   []string         `json:"commands"`
}

type NumberedTask struct {
	Key      string
	Position int
	Task     Task
}

// Problem is stable, ordered, and suitable for direct presentation by an API
// or UI. Path uses JSON-style field locations.
type Problem struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (e *ValidationError) Error() string {
	messages := make([]string, 0, len(e.Problems))
	for _, problem := range e.Problems {
		messages = append(messages, fmt.Sprintf("%s: %s", problem.Path, problem.Message))
	}
	return strings.Join(messages, "; ")
}

var drivePath = regexp.MustCompile(`^[A-Za-z]:`)

func Parse(data []byte) (Spec, error) {
	var raw rawSpec
	if err := decodeStrict(data, &raw); err != nil {
		return Spec{}, fmt.Errorf("invalid PlanSpec JSON: %w", err)
	}
	spec, err := raw.toSpec()
	if err != nil {
		return Spec{}, fmt.Errorf("invalid PlanSpec JSON: %w", err)
	}
	if err = Validate(&spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func Validate(spec *Spec) error {
	problems := ValidateProblems(spec)
	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// ValidateProblems normalizes the supplied spec and returns every problem in a
// deterministic order instead of stopping at the first failure.
func ValidateProblems(spec *Spec) []Problem {
	if spec == nil {
		return []Problem{{Code: "spec.required", Path: "$", Message: "PlanSpec is required"}}
	}
	problems := prepareSpec(spec)

	if spec.Version != CurrentVersion {
		problems = append(problems, Problem{Code: "version.unsupported", Path: "version", Message: fmt.Sprintf("version must be %d", CurrentVersion)})
	}
	if spec.Title == "" {
		problems = append(problems, Problem{Code: "title.required", Path: "title", Message: "title is required"})
	}
	if spec.Summary == "" {
		problems = append(problems, Problem{Code: "summary.required", Path: "summary", Message: "summary is required"})
	}
	if len(spec.Tasks) == 0 {
		problems = append(problems, Problem{Code: "tasks.required", Path: "tasks", Message: "at least one task is required"})
	}
	if len(spec.Tasks) > 100 {
		problems = append(problems, Problem{Code: "tasks.limit", Path: "tasks", Message: "at most 100 tasks are allowed"})
	}

	taskIndexes := make(map[string]int, len(spec.Tasks))
	acceptanceIndexes := make(map[string]string)
	for i := range spec.Tasks {
		task := &spec.Tasks[i]
		path := fmt.Sprintf("tasks[%d]", i)
		if task.Key == "" {
			problems = append(problems, Problem{Code: "task.key.required", Path: path + ".key", Message: "task key is required"})
		} else if previous, exists := taskIndexes[task.Key]; exists {
			problems = append(problems, Problem{Code: "task.key.duplicate", Path: path + ".key", Message: fmt.Sprintf("task key %q duplicates tasks[%d].key after normalization", task.Key, previous)})
		} else {
			taskIndexes[task.Key] = i
		}
		if task.Title == "" {
			problems = append(problems, Problem{Code: "task.title.required", Path: path + ".title", Message: "task title is required"})
		}
		if len(task.Scope) == 0 {
			problems = append(problems, Problem{Code: "task.scope.required", Path: path + ".scope", Message: "task scope is required"})
		}
		if len(task.AcceptanceItems) == 0 {
			problems = append(problems, Problem{Code: "acceptance.required", Path: path + ".acceptance", Message: "at least one acceptance item is required"})
		}
		for j, item := range task.AcceptanceItems {
			itemPath := fmt.Sprintf("%s.acceptance[%d]", path, j)
			if item.Key == "" || item.Description == "" {
				problems = append(problems, Problem{Code: "acceptance.incomplete", Path: itemPath, Message: "acceptance item requires both key and description"})
			}
			if item.Key != "" {
				if previous, exists := acceptanceIndexes[item.Key]; exists {
					problems = append(problems, Problem{Code: "acceptance.key.duplicate", Path: itemPath + ".key", Message: fmt.Sprintf("acceptance key %q duplicates %s after normalization", item.Key, previous)})
				} else {
					acceptanceIndexes[item.Key] = itemPath + ".key"
				}
			}
		}
	}

	for i, task := range spec.Tasks {
		for j, dependency := range task.DependsOn {
			path := fmt.Sprintf("tasks[%d].dependsOn[%d]", i, j)
			if dependency == task.Key && dependency != "" {
				problems = append(problems, Problem{Code: "dependency.self", Path: path, Message: fmt.Sprintf("task %q cannot depend on itself", task.Key)})
				continue
			}
			if _, exists := taskIndexes[dependency]; !exists {
				problems = append(problems, Problem{Code: "dependency.dangling", Path: path, Message: fmt.Sprintf("dependency %q does not reference a task in this plan", dependency)})
			}
		}
	}
	if cycle := dependencyCycle(spec.Tasks, taskIndexes); len(cycle) > 0 {
		problems = append(problems, Problem{Code: "dependency.cycle", Path: "tasks", Message: fmt.Sprintf("task dependency cycle detected: %s", strings.Join(cycle, " -> "))})
	}

	finalPath := "finalValidation"
	if len(spec.FinalValidationDefinition.Acceptance) == 0 {
		problems = append(problems, Problem{Code: "finalValidation.required", Path: finalPath + ".acceptance", Message: "final validation requires at least one acceptance item"})
	}
	for i, item := range spec.FinalValidationDefinition.Acceptance {
		itemPath := fmt.Sprintf("%s.acceptance[%d]", finalPath, i)
		if item.Key == "" || item.Description == "" {
			problems = append(problems, Problem{Code: "acceptance.incomplete", Path: itemPath, Message: "acceptance item requires both key and description"})
		}
		if item.Key != "" {
			if previous, exists := acceptanceIndexes[item.Key]; exists {
				problems = append(problems, Problem{Code: "acceptance.key.duplicate", Path: itemPath + ".key", Message: fmt.Sprintf("acceptance key %q duplicates %s after normalization", item.Key, previous)})
			} else {
				acceptanceIndexes[item.Key] = itemPath + ".key"
			}
		}
	}
	if !spec.CompatibilityMode && len(spec.FinalValidationDefinition.Commands) == 0 {
		problems = append(problems, Problem{Code: "finalValidation.commands.required", Path: finalPath + ".commands", Message: "final validation requires at least one validation command suggestion"})
	}
	return problems
}

// Problems and ValidateIssues are convenience aliases for callers that expose
// validation diagnostics without returning an error.
func Problems(spec *Spec) []Problem       { return ValidateProblems(spec) }
func ValidateIssues(spec *Spec) []Problem { return ValidateProblems(spec) }

// TopologicalOrder returns task keys in the single deterministic execution
// order. Original PlanSpec position breaks ties between simultaneously-ready
// tasks.
func TopologicalOrder(spec Spec) ([]string, error) {
	spec = cloneSpec(spec)
	if err := Validate(&spec); err != nil {
		return nil, err
	}
	indexes := make(map[string]int, len(spec.Tasks))
	indegree := make(map[string]int, len(spec.Tasks))
	dependents := make(map[string][]string, len(spec.Tasks))
	for i, task := range spec.Tasks {
		indexes[task.Key] = i
		indegree[task.Key] = len(task.DependsOn)
		for _, dependency := range task.DependsOn {
			dependents[dependency] = append(dependents[dependency], task.Key)
		}
	}
	order := make([]string, 0, len(spec.Tasks))
	for len(order) < len(spec.Tasks) {
		ready := ""
		readyIndex := len(spec.Tasks) + 1
		for _, task := range spec.Tasks {
			if indegree[task.Key] == 0 && indexes[task.Key] < readyIndex {
				ready = task.Key
				readyIndex = indexes[task.Key]
			}
		}
		if ready == "" {
			return nil, errors.New("task dependency graph has no valid topological order")
		}
		order = append(order, ready)
		indegree[ready] = -1
		for _, dependent := range dependents[ready] {
			indegree[dependent]--
		}
	}
	return order, nil
}

func Tasks(spec Spec) []NumberedTask {
	spec = cloneSpec(spec)
	_ = prepareSpec(&spec)
	order, err := TopologicalOrder(spec)
	if err != nil {
		// Callers historically passed already-trusted in-memory legacy specs. Keep
		// that path deterministic while Parse/Validate continue to reject invalid
		// external plans.
		_ = prepareSpec(&spec)
		order = make([]string, 0, len(spec.Tasks))
		for _, task := range spec.Tasks {
			order = append(order, task.Key)
		}
	}
	byKey := make(map[string]Task, len(spec.Tasks))
	for _, task := range spec.Tasks {
		byKey[task.Key] = task
	}
	out := make([]NumberedTask, 0, len(spec.Tasks)+1)
	for i, key := range order {
		out = append(out, NumberedTask{Key: key, Position: i + 1, Task: byKey[key]})
	}

	finalKey := "FINAL"
	if spec.CompatibilityMode {
		finalKey = fmt.Sprintf("P%03d", len(out)+1)
	} else {
		used := make(map[string]bool, len(spec.Tasks))
		for _, task := range spec.Tasks {
			used[task.Key] = true
		}
		for suffix := 2; used[finalKey]; suffix++ {
			finalKey = fmt.Sprintf("FINAL-%d", suffix)
		}
	}
	finalAcceptance := acceptanceDescriptions(spec.FinalValidationDefinition.Acceptance)
	validation := Task{
		Key:                finalKey,
		Title:              "Final validation",
		DependsOn:          append([]string(nil), order...),
		Scope:              []string{"."},
		Acceptance:         finalAcceptance,
		AcceptanceItems:    append([]AcceptanceItem(nil), spec.FinalValidationDefinition.Acceptance...),
		ValidationCommands: append([]string(nil), spec.FinalValidationDefinition.Commands...),
	}
	out = append(out, NumberedTask{Key: finalKey, Position: len(out) + 1, Task: validation})
	return out
}

func Render(spec Spec) string {
	spec = cloneSpec(spec)
	_ = prepareSpec(&spec)
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n> PlanSpec v%d · %s\n\n## Summary\n\n%s\n\n## Tasks\n", spec.Title, spec.Version, compatibilityLabel(spec.CompatibilityMode), spec.Summary)
	for _, item := range Tasks(spec) {
		fmt.Fprintf(&b, "\n### %s · %s\n", item.Key, item.Task.Title)
		renderList(&b, "Dependencies", item.Task.DependsOn, false, false)
		renderList(&b, "Scope", item.Task.Scope, true, false)
		renderList(&b, "Inputs", item.Task.Inputs, false, false)
		renderList(&b, "Outputs", item.Task.Outputs, false, false)
		renderList(&b, "Risks", item.Task.Risks, false, false)
		b.WriteString("\n**Acceptance**\n")
		for _, acceptance := range item.Task.AcceptanceItems {
			fmt.Fprintf(&b, "- [ ] `%s` %s\n", acceptance.Key, acceptance.Description)
		}
		renderList(&b, "Suggested validation commands", item.Task.ValidationCommands, true, true)
	}
	return b.String()
}

func (s *Spec) UnmarshalJSON(data []byte) error {
	var raw rawSpec
	if err := decodeStrict(data, &raw); err != nil {
		return err
	}
	spec, err := raw.toSpec()
	if err != nil {
		return err
	}
	*s = spec
	return nil
}

func (s Spec) MarshalJSON() ([]byte, error) {
	copy := cloneSpec(s)
	_ = prepareSpec(&copy)
	type encodedSpec struct {
		Version           int                  `json:"version"`
		CompatibilityMode bool                 `json:"compatibilityMode"`
		Title             string               `json:"title"`
		Summary           string               `json:"summary"`
		Tasks             []Task               `json:"tasks"`
		FinalValidation   ValidationDefinition `json:"finalValidation"`
	}
	return json.Marshal(encodedSpec{
		Version: copy.Version, CompatibilityMode: copy.CompatibilityMode,
		Title: copy.Title, Summary: copy.Summary, Tasks: copy.Tasks,
		FinalValidation: copy.FinalValidationDefinition,
	})
}

func (t Task) MarshalJSON() ([]byte, error) {
	copy := t
	prepareTask(&copy, 0, &Spec{CompatibilityMode: true})
	type encodedTask struct {
		Key                string           `json:"key"`
		Title              string           `json:"title"`
		DependsOn          []string         `json:"dependsOn"`
		Scope              []string         `json:"scope"`
		Inputs             []string         `json:"inputs"`
		Outputs            []string         `json:"outputs"`
		Risks              []string         `json:"risks"`
		Acceptance         []AcceptanceItem `json:"acceptance"`
		ValidationCommands []string         `json:"validationCommands"`
	}
	return json.Marshal(encodedTask{
		Key: copy.Key, Title: copy.Title, DependsOn: nonNil(copy.DependsOn), Scope: nonNil(copy.Scope),
		Inputs: nonNil(copy.Inputs), Outputs: nonNil(copy.Outputs), Risks: nonNil(copy.Risks),
		Acceptance: nonNilAcceptance(copy.AcceptanceItems), ValidationCommands: nonNil(copy.ValidationCommands),
	})
}

type rawSpec struct {
	Version           *int            `json:"version"`
	CompatibilityMode *bool           `json:"compatibilityMode"`
	Title             string          `json:"title"`
	Summary           string          `json:"summary"`
	Tasks             []rawTask       `json:"tasks"`
	FinalValidation   json.RawMessage `json:"finalValidation"`
}

type rawTask struct {
	Key                *string         `json:"key"`
	Title              string          `json:"title"`
	DependsOn          *[]string       `json:"dependsOn"`
	Scope              []string        `json:"scope"`
	Inputs             *[]string       `json:"inputs"`
	Outputs            *[]string       `json:"outputs"`
	Risks              *[]string       `json:"risks"`
	Acceptance         json.RawMessage `json:"acceptance"`
	ValidationCommands *[]string       `json:"validationCommands"`
}

type rawValidationDefinition struct {
	Acceptance json.RawMessage `json:"acceptance"`
	Commands   *[]string       `json:"commands"`
}

func (raw rawSpec) toSpec() (Spec, error) {
	spec := Spec{Title: raw.Title, Summary: raw.Summary}
	if raw.Version == nil {
		spec.Version = CurrentVersion
		spec.CompatibilityMode = true
	} else {
		spec.Version = *raw.Version
	}
	if raw.CompatibilityMode == nil {
		spec.CompatibilityMode = true
	} else if *raw.CompatibilityMode {
		spec.CompatibilityMode = true
	}
	if raw.Tasks == nil {
		spec.Tasks = nil
	} else {
		spec.Tasks = make([]Task, 0, len(raw.Tasks))
	}
	for i, item := range raw.Tasks {
		task := Task{Title: item.Title, Scope: item.Scope}
		if item.Key == nil {
			spec.CompatibilityMode = true
		} else {
			task.Key = *item.Key
		}
		if item.DependsOn == nil {
			spec.CompatibilityMode = true
		} else {
			task.DependsOn = *item.DependsOn
		}
		if item.Inputs == nil {
			spec.CompatibilityMode = true
		} else {
			task.Inputs = *item.Inputs
		}
		if item.Outputs == nil {
			spec.CompatibilityMode = true
		} else {
			task.Outputs = *item.Outputs
		}
		if item.Risks == nil {
			spec.CompatibilityMode = true
		} else {
			task.Risks = *item.Risks
		}
		if item.ValidationCommands == nil {
			spec.CompatibilityMode = true
		} else {
			task.ValidationCommands = *item.ValidationCommands
		}
		items, legacy, err := decodeAcceptance(item.Acceptance)
		if err != nil {
			return Spec{}, fmt.Errorf("tasks[%d].acceptance: %w", i, err)
		}
		if legacy {
			spec.CompatibilityMode = true
			task.Acceptance = acceptanceDescriptions(items)
		} else {
			task.AcceptanceItems = items
		}
		spec.Tasks = append(spec.Tasks, task)
	}

	trimmedFinal := bytes.TrimSpace(raw.FinalValidation)
	if len(trimmedFinal) == 0 || bytes.Equal(trimmedFinal, []byte("null")) {
		return spec, nil
	}
	if trimmedFinal[0] == '[' {
		items, legacy, err := decodeAcceptance(raw.FinalValidation)
		if err != nil {
			return Spec{}, fmt.Errorf("finalValidation: %w", err)
		}
		spec.CompatibilityMode = spec.CompatibilityMode || legacy
		if legacy {
			spec.FinalValidation = acceptanceDescriptions(items)
		} else {
			spec.FinalValidationDefinition.Acceptance = items
		}
		return spec, nil
	}
	var definition rawValidationDefinition
	if err := decodeStrict(raw.FinalValidation, &definition); err != nil {
		return Spec{}, fmt.Errorf("finalValidation: %w", err)
	}
	items, legacy, err := decodeAcceptance(definition.Acceptance)
	if err != nil {
		return Spec{}, fmt.Errorf("finalValidation.acceptance: %w", err)
	}
	if legacy || definition.Commands == nil {
		spec.CompatibilityMode = true
	}
	if legacy {
		spec.FinalValidation = acceptanceDescriptions(items)
	} else {
		spec.FinalValidationDefinition.Acceptance = items
	}
	if definition.Commands != nil {
		spec.FinalValidationDefinition.Commands = *definition.Commands
	}
	return spec, nil
}

func decodeAcceptance(data json.RawMessage) ([]AcceptanceItem, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, false, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, false, errors.New("must be an array")
	}
	items := make([]AcceptanceItem, 0, len(entries))
	legacy := false
	structured := false
	for i, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) > 0 && entry[0] == '"' {
			if structured {
				return nil, false, errors.New("must not mix string and structured acceptance items")
			}
			var description string
			if err := json.Unmarshal(entry, &description); err != nil {
				return nil, false, fmt.Errorf("item %d must be a string or structured acceptance item", i)
			}
			items = append(items, AcceptanceItem{Description: description})
			legacy = true
			continue
		}
		if legacy {
			return nil, false, errors.New("must not mix string and structured acceptance items")
		}
		structured = true
		var item AcceptanceItem
		if err := decodeStrict(entry, &item); err != nil {
			return nil, false, fmt.Errorf("item %d: %w", i, err)
		}
		items = append(items, item)
	}
	return items, legacy, nil
}

func prepareSpec(spec *Spec) []Problem {
	problems := []Problem{}
	if spec.Version == 0 || spec.Version == 1 {
		spec.Version = CurrentVersion
		spec.CompatibilityMode = true
	}
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Summary = strings.TrimSpace(spec.Summary)
	for i := range spec.Tasks {
		prepareTask(&spec.Tasks[i], i, spec)
		paths, invalid := normalizePaths(spec.Tasks[i].Scope)
		spec.Tasks[i].Scope = paths
		for _, value := range invalid {
			problems = append(problems, Problem{Code: "task.scope.invalid", Path: fmt.Sprintf("tasks[%d].scope", i), Message: fmt.Sprintf("scope path %q must stay within the workspace", value)})
		}
	}
	if spec.FinalValidationDefinition.Acceptance == nil && spec.FinalValidation != nil {
		spec.CompatibilityMode = true
		spec.FinalValidationDefinition.Acceptance = make([]AcceptanceItem, 0, len(spec.FinalValidation))
		for i, description := range spec.FinalValidation {
			spec.FinalValidationDefinition.Acceptance = append(spec.FinalValidationDefinition.Acceptance, AcceptanceItem{
				Key: fmt.Sprintf("FINAL-A%03d", i+1), Description: description,
			})
		}
	}
	prepareAcceptance(spec.FinalValidationDefinition.Acceptance)
	spec.FinalValidationDefinition.Commands = normalizeList(spec.FinalValidationDefinition.Commands, false)
	spec.FinalValidation = acceptanceDescriptions(spec.FinalValidationDefinition.Acceptance)
	return problems
}

func prepareTask(task *Task, index int, spec *Spec) {
	if strings.TrimSpace(task.Key) == "" && spec.CompatibilityMode {
		task.Key = fmt.Sprintf("P%03d", index+1)
	}
	task.Key = normalizeKey(task.Key)
	task.Title = strings.TrimSpace(task.Title)
	if task.DependsOn == nil {
		spec.CompatibilityMode = true
		if index > 0 {
			task.DependsOn = []string{spec.Tasks[index-1].Key}
		} else {
			task.DependsOn = []string{}
		}
	}
	for i := range task.DependsOn {
		task.DependsOn[i] = normalizeKey(task.DependsOn[i])
	}
	task.DependsOn = normalizeList(task.DependsOn, false)
	task.Inputs = normalizeListField(task.Inputs, spec)
	task.Outputs = normalizeListField(task.Outputs, spec)
	task.Risks = normalizeListField(task.Risks, spec)
	task.ValidationCommands = normalizeListField(task.ValidationCommands, spec)
	if task.AcceptanceItems == nil && task.Acceptance != nil {
		spec.CompatibilityMode = true
		task.AcceptanceItems = make([]AcceptanceItem, 0, len(task.Acceptance))
		for i, description := range task.Acceptance {
			task.AcceptanceItems = append(task.AcceptanceItems, AcceptanceItem{
				Key: fmt.Sprintf("%s-A%03d", task.Key, i+1), Description: description,
			})
		}
	}
	prepareAcceptance(task.AcceptanceItems)
	task.Acceptance = acceptanceDescriptions(task.AcceptanceItems)
}

func normalizeListField(values []string, spec *Spec) []string {
	if values == nil {
		spec.CompatibilityMode = true
		return []string{}
	}
	return normalizeList(values, false)
}

func prepareAcceptance(items []AcceptanceItem) {
	for i := range items {
		items[i].Key = normalizeKey(items[i].Key)
		items[i].Description = strings.TrimSpace(items[i].Description)
		// Keys for legacy strings are assigned before this function. Structured
		// items deliberately keep a missing key so validation can report it.
	}
}

func dependencyCycle(tasks []Task, indexes map[string]int) []string {
	state := make(map[string]uint8, len(tasks))
	stack := make([]string, 0, len(tasks))
	stackIndex := make(map[string]int, len(tasks))
	var cycle []string
	var visit func(string) bool
	visit = func(key string) bool {
		state[key] = 1
		stackIndex[key] = len(stack)
		stack = append(stack, key)
		for _, dependency := range tasks[indexes[key]].DependsOn {
			if dependency == key {
				continue
			}
			if _, exists := indexes[dependency]; !exists {
				continue
			}
			switch state[dependency] {
			case 0:
				if visit(dependency) {
					return true
				}
			case 1:
				start := stackIndex[dependency]
				cycle = append(append([]string(nil), stack[start:]...), dependency)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, key)
		state[key] = 2
		return false
	}
	for _, task := range tasks {
		if task.Key != "" && state[task.Key] == 0 {
			if visit(task.Key) {
				return cycle
			}
		}
	}
	return nil
}

func normalizeKey(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	separator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if separator && b.Len() > 0 {
				b.WriteByte('-')
			}
			separator = false
			b.WriteRune(unicode.ToUpper(r))
			continue
		}
		separator = true
	}
	return strings.Trim(b.String(), "-")
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

func normalizePaths(values []string) ([]string, []string) {
	valid := make([]string, 0, len(values))
	invalid := []string{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		value = strings.ReplaceAll(value, "\\", "/")
		value = strings.TrimPrefix(filepath.Clean(value), "./")
		if strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") || drivePath.MatchString(value) {
			invalid = append(invalid, raw)
			continue
		}
		valid = append(valid, value)
	}
	return normalizeList(valid, true), invalid
}

func acceptanceDescriptions(items []AcceptanceItem) []string {
	if items == nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Description)
	}
	return out
}

func renderList(b *strings.Builder, title string, values []string, code, commands bool) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "\n**%s**\n", title)
	for _, value := range values {
		if code || commands {
			fmt.Fprintf(b, "- `%s`\n", value)
		} else {
			fmt.Fprintf(b, "- %s\n", value)
		}
	}
}

func compatibilityLabel(compatibility bool) string {
	if compatibility {
		return "兼容模式"
	}
	return "结构化模式"
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple JSON values are not allowed")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("multiple JSON values are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func cloneSpec(spec Spec) Spec {
	copy := spec
	copy.Tasks = make([]Task, len(spec.Tasks))
	for i, task := range spec.Tasks {
		copy.Tasks[i] = task
		copy.Tasks[i].DependsOn = cloneStrings(task.DependsOn)
		copy.Tasks[i].Scope = cloneStrings(task.Scope)
		copy.Tasks[i].Inputs = cloneStrings(task.Inputs)
		copy.Tasks[i].Outputs = cloneStrings(task.Outputs)
		copy.Tasks[i].Risks = cloneStrings(task.Risks)
		copy.Tasks[i].Acceptance = cloneStrings(task.Acceptance)
		copy.Tasks[i].AcceptanceItems = cloneAcceptance(task.AcceptanceItems)
		copy.Tasks[i].ValidationCommands = cloneStrings(task.ValidationCommands)
	}
	copy.FinalValidation = cloneStrings(spec.FinalValidation)
	copy.FinalValidationDefinition.Acceptance = cloneAcceptance(spec.FinalValidationDefinition.Acceptance)
	copy.FinalValidationDefinition.Commands = cloneStrings(spec.FinalValidationDefinition.Commands)
	return copy
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func cloneAcceptance(values []AcceptanceItem) []AcceptanceItem {
	if values == nil {
		return nil
	}
	return append([]AcceptanceItem{}, values...)
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilAcceptance(values []AcceptanceItem) []AcceptanceItem {
	if values == nil {
		return []AcceptanceItem{}
	}
	return values
}
