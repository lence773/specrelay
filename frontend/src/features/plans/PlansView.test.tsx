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

const { getPlan, runPlan, runTask } = vi.hoisted(() => ({
  getPlan: vi.fn(),
  runPlan: vi.fn(),
  runTask: vi.fn(),
}));
vi.mock("../../api/client", () => ({
  api: { plan: getPlan, runPlan, runTask },
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
    runPlan.mockReset();
    runTask.mockReset();
    getPlan.mockResolvedValue({ plan, tasks: [task] });
    runPlan.mockResolvedValue({});
    runTask.mockResolvedValue({});
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

    await waitFor(() =>
      expect(runPlan).toHaveBeenCalledWith(plan, "claude"),
    );
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
    fireEvent.click(
      within(taskCard).getByRole("button", { name: "运行任务" }),
    );

    await waitFor(() => expect(runTask).toHaveBeenCalledWith(task, "codex"));
  });
});
