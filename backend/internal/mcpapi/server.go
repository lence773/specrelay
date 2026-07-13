package mcpapi

import (
	"context"
	"github.com/google/uuid"
	"github.com/lyming99/specrelay/backend/internal/app"
	"github.com/lyming99/specrelay/backend/internal/repository"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
)

type Output struct {
	Data any `json:"data"`
}
type ProjectInput struct {
	Name          string `json:"name" jsonschema:"Project name"`
	Description   string `json:"description,omitempty"`
	WorkspacePath string `json:"workspacePath" jsonschema:"Existing local workspace directory"`
}
type ProjectIDInput struct {
	ProjectID string `json:"projectId" jsonschema:"Project UUID"`
}
type AutomationInput struct {
	ProjectID string `json:"projectId"`
	Version   int64  `json:"version"`
}
type IntakeInput struct {
	ProjectID      string `json:"projectId"`
	Kind           string `json:"kind" jsonschema:"requirement or feedback"`
	ParentIntakeID string `json:"parentIntakeId,omitempty"`
	Title          string `json:"title"`
	Body           string `json:"body"`
}
type PlanInput struct {
	PlanID string `json:"planId"`
}
type PlanActionInput struct {
	PlanID  string `json:"planId"`
	Version int64  `json:"version"`
}
type TaskInput struct {
	TaskID  string `json:"taskId"`
	Version int64  `json:"version"`
}

func Handler(service *app.Service, store *repository.Store) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "specrelay", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "projects_list", Description: "List SpecRelay projects"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, Output, error) {
		items, err := store.ListProjects(ctx)
		return nil, Output{Data: items}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "projects_create", Description: "Create an SpecRelay project bound to an existing local workspace"}, func(ctx context.Context, req *mcp.CallToolRequest, in ProjectInput) (*mcp.CallToolResult, Output, error) {
		item, err := service.CreateProject(ctx, in.Name, in.Description, in.WorkspacePath)
		return nil, Output{Data: item}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "intakes_list", Description: "List requirements and feedback for a project"}, func(ctx context.Context, req *mcp.CallToolRequest, in ProjectIDInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.ProjectID)
		if err != nil {
			return nil, Output{}, err
		}
		items, err := store.ListIntakes(ctx, id)
		return nil, Output{Data: items}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "intakes_create", Description: "Create a requirement or feedback; automation queues planning when enabled"}, func(ctx context.Context, req *mcp.CallToolRequest, in IntakeInput) (*mcp.CallToolResult, Output, error) {
		projectID, err := uuid.Parse(in.ProjectID)
		if err != nil {
			return nil, Output{}, err
		}
		var parent *uuid.UUID
		if in.ParentIntakeID != "" {
			id, e := uuid.Parse(in.ParentIntakeID)
			if e != nil {
				return nil, Output{}, e
			}
			parent = &id
		}
		item, job, err := service.CreateIntake(ctx, repository.CreateIntakeParams{ProjectID: projectID, Kind: in.Kind, ParentIntakeID: parent, Title: in.Title, Body: in.Body})
		return nil, Output{Data: map[string]any{"intake": item, "job": job}}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "plans_list", Description: "List plans for a project"}, func(ctx context.Context, req *mcp.CallToolRequest, in ProjectIDInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.ProjectID)
		if err != nil {
			return nil, Output{}, err
		}
		items, err := store.ListPlans(ctx, id)
		return nil, Output{Data: items}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "plan_get", Description: "Get a plan and its ordered tasks"}, func(ctx context.Context, req *mcp.CallToolRequest, in PlanInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.PlanID)
		if err != nil {
			return nil, Output{}, err
		}
		plan, err := store.GetPlan(ctx, id)
		if err != nil {
			return nil, Output{}, err
		}
		tasks, err := store.ListTasks(ctx, id)
		return nil, Output{Data: map[string]any{"plan": plan, "tasks": tasks}}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "automation_start", Description: "Start project automation using optimistic version"}, func(ctx context.Context, req *mcp.CallToolRequest, in AutomationInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.ProjectID)
		if err != nil {
			return nil, Output{}, err
		}
		item, err := store.SetAutomation(ctx, id, true, in.Version)
		return nil, Output{Data: item}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "automation_stop", Description: "Stop project automation and queued work"}, func(ctx context.Context, req *mcp.CallToolRequest, in AutomationInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.ProjectID)
		if err != nil {
			return nil, Output{}, err
		}
		item, err := store.SetAutomation(ctx, id, false, in.Version)
		if err == nil {
			service.Runner.CancelPrefix(id.String() + ":")
		}
		return nil, Output{Data: item}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "plan_run", Description: "Run a ready or blocked plan"}, func(ctx context.Context, req *mcp.CallToolRequest, in PlanActionInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.PlanID)
		if err != nil {
			return nil, Output{}, err
		}
		job, err := service.QueuePlan(repository.WithExecutionProvider(ctx, ""), id, in.Version, "")
		return nil, Output{Data: job}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "plan_stop", Description: "Stop a running plan"}, func(ctx context.Context, req *mcp.CallToolRequest, in PlanActionInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.PlanID)
		if err != nil {
			return nil, Output{}, err
		}
		plan, jobs, err := service.StopPlan(ctx, id, in.Version)
		return nil, Output{Data: map[string]any{"plan": plan, "jobIds": jobs}}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "task_run", Description: "Queue a pending task"}, func(ctx context.Context, req *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.TaskID)
		if err != nil {
			return nil, Output{}, err
		}
		job, err := service.QueueTask(repository.WithExecutionProvider(ctx, ""), id, in.Version, "")
		return nil, Output{Data: job}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "task_retry", Description: "Retry a failed or cancelled task"}, func(ctx context.Context, req *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.TaskID)
		if err != nil {
			return nil, Output{}, err
		}
		job, err := service.QueueTask(repository.WithExecutionProvider(ctx, ""), id, in.Version, "")
		return nil, Output{Data: job}, err
	})
	mcp.AddTool(server, &mcp.Tool{Name: "task_stop", Description: "Stop a queued or running task"}, func(ctx context.Context, req *mcp.CallToolRequest, in TaskInput) (*mcp.CallToolResult, Output, error) {
		id, err := uuid.Parse(in.TaskID)
		if err != nil {
			return nil, Output{}, err
		}
		task, jobs, err := service.StopTask(ctx, id, in.Version)
		return nil, Output{Data: map[string]any{"task": task, "jobIds": jobs}}, err
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}
