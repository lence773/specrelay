// @vitest-environment jsdom
import "@testing-library/jest-dom/vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Plan, PlanTask, Project } from "../../api/types";

const {
  getPlan,
  getTask,
  runPlan,
  runTask,
  createFeedback,
  checkpointDiff,
  getFeedback,
} = vi.hoisted(() => ({
  getPlan: vi.fn(),
  getTask: vi.fn(),
  runPlan: vi.fn(),
  runTask: vi.fn(),
  createFeedback: vi.fn(),
  checkpointDiff: vi.fn(),
  getFeedback: vi.fn(),
}));
vi.mock("../../api/client", () => ({
  api: {
    plan: getPlan,
    task: getTask,
    runPlan,
    runTask,
    createFeedback,
    checkpointDiff,
    feedback: getFeedback,
  },
}));

import { PlansView } from "./PlansView";

const project = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "计划测试项目",
  description: "",
  workspacePath: "/tmp/plan-test",
  automationEnabled: false,
  createdAt: "2026-07-14T00:00:00Z",
  updatedAt: "2026-07-14T00:00:00Z",
  version: 1,
} as Project;

const plan = {
  id: "22222222-2222-4222-8222-222222222222",
  projectId: project.id,
  intakeId: "33333333-3333-4333-8333-333333333333",
  title: "固定顶部计划",
  spec: {
    title: "固定顶部计划",
    summary: "验证计划详情结构",
    tasks: [
      {
        title: "实现固定结构",
        scope: ["frontend/src/features/plans"],
        acceptance: ["顶部操作保持可见"],
      },
    ],
    finalValidation: ["运行前端测试"],
  },
  markdown: "# 固定顶部计划\n\n计划文档正文",
  status: "ready",
  createdAt: "2026-07-14T00:00:00Z",
  updatedAt: "2026-07-14T00:00:00Z",
  version: 2,
} as Plan;

const task = {
  id: "44444444-4444-4444-8444-444444444444",
  projectId: project.id,
  planId: plan.id,
  taskKey: "P001",
  position: 1,
  title: "实现固定结构",
  scope: ["frontend/src/features/plans"],
  acceptance: ["顶部操作保持可见"],
  status: "pending",
  createdAt: "2026-07-14T00:00:00Z",
  updatedAt: "2026-07-14T00:00:00Z",
  version: 3,
} as PlanTask;

function renderPlans() {
  return render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <PlansView project={project} plans={[plan]} />
    </QueryClientProvider>,
  );
}

describe("PlansView detail structure and actions", () => {
  afterEach(cleanup);

  beforeEach(() => {
    getPlan.mockReset();
    getTask.mockReset();
    runPlan.mockReset();
    runTask.mockReset();
    createFeedback.mockReset();
    checkpointDiff.mockReset();
    getFeedback.mockReset();
    getFeedback.mockResolvedValue({
      feedback: {
        id: "feedback-created",
        title: "新反馈",
        body: "反馈说明",
        status: "open",
        createdAt: "2026-07-15T00:00:00Z",
        updatedAt: "2026-07-15T00:00:00Z",
      },
      requirement: {
        id: plan.intakeId,
        title: "父需求",
        body: "需求说明",
        status: "planned",
        createdAt: "2026-07-14T00:00:00Z",
        updatedAt: "2026-07-15T00:00:00Z",
      },
      association: {
        requirementId: plan.intakeId,
        planId: plan.id,
        taskId: task.id,
      },
      plan: {
        id: plan.id,
        title: plan.title,
        status: plan.status,
        markdown: plan.markdown,
      },
      task: {
        id: task.id,
        taskKey: task.taskKey,
        title: task.title,
        status: task.status,
        acceptance: "[]",
        acceptanceStatus: "pending",
        acceptanceResult: "{}",
      },
      revision: { currentStatus: "not_started", items: [] },
    });
    getPlan.mockResolvedValue({ plan, tasks: [task], feedback: [] });
    getTask.mockResolvedValue({ task, feedback: [] });
    runPlan.mockResolvedValue({});
    runTask.mockResolvedValue({});
    createFeedback.mockResolvedValue({
      feedback: { ...task, id: "feedback-created", kind: "feedback" },
    });
  });

  it("keeps the title, plan actions, and progress fixed above the scrollable plan content", async () => {
    const { container } = renderPlans();

    await screen.findByRole("heading", { name: plan.title, level: 2 });

    const detail = container.querySelector<HTMLElement>(".plan-detail")!;
    const topHeader = detail.querySelector<HTMLElement>(":scope > header")!;
    const planActions = screen.getByRole("region", { name: "运行整份计划" });
    const progress = detail.querySelector<HTMLElement>(":scope > .progress")!;
    const scroll = container.querySelector<HTMLElement>(
      '[aria-label="计划内容"]',
    )!;

    expect(detail).toBeInTheDocument();
    expect(topHeader.parentElement).toBe(detail);
    expect(planActions.parentElement).toBe(detail);
    expect(progress.parentElement).toBe(detail);
    expect(scroll.parentElement).toBe(detail);
    expect(
      within(topHeader).getByRole("heading", { name: plan.title }),
    ).toBeInTheDocument();
    expect(
      within(planActions).getByText("整份计划执行提供方"),
    ).toBeInTheDocument();
    expect(
      within(planActions).getByRole("button", { name: "运行计划" }),
    ).toBeInTheDocument();
    expect(progress).toBeInTheDocument();
    expect(scroll).not.toContainElement(topHeader);
    expect(scroll).not.toContainElement(planActions);
    expect(scroll).not.toContainElement(progress);

    const taskTitle = within(scroll).getByText("P001 · 实现固定结构");
    const taskCard = taskTitle.closest("article");
    expect(taskCard).toBeInTheDocument();
    expect(
      within(scroll).getByText("查看渲染后的计划文档"),
    ).toBeInTheDocument();

    const taskProvider = within(taskCard as HTMLElement).getByText(
      "P001 本次执行提供方",
    );
    const taskAction = within(taskCard as HTMLElement).getByRole("button", {
      name: "运行任务",
    });
    expect(taskProvider).toBeInTheDocument();
    expect(taskAction).toBeInTheDocument();
    expect(scroll).toContainElement(taskCard);
    expect(scroll).toContainElement(taskProvider);
    expect(scroll).toContainElement(taskAction);
    expect(planActions).not.toContainElement(taskProvider);
    expect(planActions).not.toContainElement(taskAction);
  });

  it("passes the selected CLI providers to plan and task execution", async () => {
    renderPlans();

    const planActions = await screen.findByRole("region", {
      name: "运行整份计划",
    });
    fireEvent.click(
      within(planActions).getByRole("button", {
        name: "整份计划执行提供方：Claude CLI",
      }),
    );
    fireEvent.click(
      within(planActions).getByRole("button", { name: "运行计划" }),
    );

    await waitFor(() => expect(runPlan).toHaveBeenCalledWith(plan, "claude"));
    await waitFor(() =>
      expect(
        within(planActions).getByRole("button", { name: "运行计划" }),
      ).toBeEnabled(),
    );

    const taskCard = screen
      .getByText("P001 · 实现固定结构")
      .closest("article") as HTMLElement;
    fireEvent.click(
      within(taskCard).getByRole("button", {
        name: "P001 本次执行提供方：Codex CLI",
      }),
    );
    fireEvent.click(within(taskCard).getByRole("button", { name: "运行任务" }));

    await waitFor(() => expect(runTask).toHaveBeenCalledWith(task, "codex"));
  });

  it("prefills the requirement, plan, and task when feedback starts from a task card", async () => {
    renderPlans();

    const taskCard = (await screen.findByText("P001 · 实现固定结构")).closest(
      "article",
    ) as HTMLElement;
    fireEvent.click(within(taskCard).getByRole("button", { name: "创建反馈" }));

    const dialog = screen.getByRole("dialog", { name: "创建关联反馈" });
    expect(within(dialog).getByText("提交前检查关联对象")).toBeInTheDocument();
    expect(within(dialog).getByText(plan.title)).toBeInTheDocument();
    expect(within(dialog).getByText("P001 · 实现固定结构")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText("反馈标题"), {
      target: { value: "任务反馈" },
    });
    fireEvent.change(within(dialog).getByLabelText("反馈说明"), {
      target: { value: "请调整任务实现" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "创建反馈" }));

    await waitFor(() =>
      expect(createFeedback).toHaveBeenCalledWith(project.id, {
        requirementId: plan.intakeId,
        planId: plan.id,
        taskId: task.id,
        title: "任务反馈",
        body: "请调整任务实现",
      }),
    );
  });

  it("selects a bounded Diff line range and creates precise feedback", async () => {
    const checkpointId = "55555555-5555-4555-8555-555555555555";
    const checkpointTask = {
      ...task,
      status: "succeeded",
      checkpointId,
    } as PlanTask & { checkpointId: string };
    const fileId = "66666666-6666-4666-8666-666666666666";
    const hunkId = "77777777-7777-4777-8777-777777777777";
    getPlan.mockResolvedValue({ plan, tasks: [checkpointTask], feedback: [] });
    getTask.mockResolvedValue({ task: checkpointTask, feedback: [] });
    checkpointDiff.mockResolvedValue({
      checkpoint: {
        id: checkpointId,
        projectId: project.id,
        planId: plan.id,
        intakeId: plan.intakeId,
        taskId: task.id,
        sequence: 3,
        kind: "task_checkpoint",
        changeSummary: {},
        additions: 2,
        deletions: 1,
        createdAt: "2026-07-15T00:00:00Z",
        files: [
          {
            id: fileId,
            snapshotId: checkpointId,
            sequence: 1,
            path: "frontend/src/App.tsx",
            status: "modified",
            staged: false,
            binary: false,
            additions: 2,
            deletions: 1,
            createdAt: "2026-07-15T00:00:00Z",
            hunks: [
              {
                id: hunkId,
                fileId,
                sequence: 1,
                header: "@@ -10,2 +10,3 @@",
                patch: "@@ -10,2 +10,3 @@\n same\n-old\n+new\n+extra",
                oldStartLine: 10,
                oldLineCount: 2,
                newStartLine: 10,
                newLineCount: 3,
                createdAt: "2026-07-15T00:00:00Z",
              },
            ],
          },
        ],
      },
      feedback: [],
    });
    renderPlans();

    const taskCard = (await screen.findByText("P001 · 实现固定结构")).closest(
      "article",
    ) as HTMLElement;
    fireEvent.click(
      within(taskCard).getByRole("button", { name: "查看变更与 Diff" }),
    );
    await screen.findByRole("table", { name: "frontend/src/App.tsx Diff" });
    fireEvent.click(screen.getByRole("button", { name: "选择新行 11" }));
    fireEvent.click(screen.getByRole("button", { name: "选择新行 12" }));
    fireEvent.click(
      screen.getByRole("button", { name: "为所选行创建精确反馈" }),
    );

    const dialog = screen.getByRole("dialog", { name: "创建关联反馈" });
    expect(
      within(dialog).getByText("精确范围：新文件第 11–12 行"),
    ).toBeInTheDocument();
    fireEvent.change(within(dialog).getByLabelText("反馈标题"), {
      target: { value: "精确 Diff 反馈" },
    });
    fireEvent.change(within(dialog).getByLabelText("反馈说明"), {
      target: { value: "这两行需要调整" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "创建反馈" }));

    await waitFor(() =>
      expect(createFeedback).toHaveBeenCalledWith(project.id, {
        requirementId: plan.intakeId,
        planId: plan.id,
        taskId: task.id,
        checkpointId,
        fileId,
        diffHunkId: hunkId,
        diffLineSide: "new",
        diffLineStart: 11,
        diffLineEnd: 12,
        title: "精确 Diff 反馈",
        body: "这两行需要调整",
      }),
    );
  });

  it("shows reverse feedback and revision status on plan and task views", async () => {
    const reference = {
      id: "feedback-1",
      requirementId: plan.intakeId,
      title: "需要修订交互",
      feedbackStatus: "open",
      revisionStatus: "ready",
      createdAt: "2026-07-15T00:00:00Z",
    };
    getPlan.mockResolvedValue({ plan, tasks: [task], feedback: [reference] });
    getTask.mockResolvedValue({ task, feedback: [reference] });
    renderPlans();

    await screen.findByText("计划关联反馈（1）");
    expect(screen.getAllByText("需要修订交互").length).toBeGreaterThan(0);
    expect(screen.getAllByText("修订计划就绪").length).toBeGreaterThan(0);
    expect(screen.getByText("关联反馈 1")).toBeInTheDocument();
  });

  it("renders a clear invalid checkpoint message and disables broken Diff actions", async () => {
    const checkpointTask = {
      ...task,
      checkpointId: "missing-checkpoint",
    } as PlanTask & { checkpointId: string };
    getPlan.mockResolvedValue({ plan, tasks: [checkpointTask], feedback: [] });
    getTask.mockResolvedValue({ task: checkpointTask, feedback: [] });
    checkpointDiff.mockRejectedValue({ status: 404, message: "not found" });
    renderPlans();

    const taskCard = (await screen.findByText("P001 · 实现固定结构")).closest(
      "article",
    ) as HTMLElement;
    fireEvent.click(
      within(taskCard).getByRole("button", { name: "查看变更与 Diff" }),
    );

    expect(await screen.findByText("关联对象已不存在")).toBeInTheDocument();
    expect(screen.getByText(/损坏链接和修订操作已禁用/)).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "为所选行创建精确反馈" }),
    ).not.toBeInTheDocument();
  });
});
