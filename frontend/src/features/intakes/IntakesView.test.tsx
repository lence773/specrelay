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
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { FeedbackContext, Intake, Project } from "../../api/types";

const {
  createIntake,
  discussRequirement,
  generatePlan,
  upload,
  createFeedback,
  getFeedback,
  discussFeedback,
  createFeedbackRevision,
  generateFeedbackRevisionPlan,
  checkpointDiff,
} = vi.hoisted(() => ({
  createIntake: vi.fn(),
  discussRequirement: vi.fn(),
  generatePlan: vi.fn(),
  upload: vi.fn(),
  createFeedback: vi.fn(),
  getFeedback: vi.fn(),
  discussFeedback: vi.fn(),
  createFeedbackRevision: vi.fn(),
  generateFeedbackRevisionPlan: vi.fn(),
  checkpointDiff: vi.fn(),
}));
vi.mock("../../api/client", () => ({
  api: {
    createIntake,
    discussRequirement,
    generatePlan,
    upload,
    createFeedback,
    feedback: getFeedback,
    discussFeedback,
    createFeedbackRevision,
    generateFeedbackRevisionPlan,
    checkpointDiff,
  },
}));

import { IntakesView } from "./IntakesView";

const project = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "缓存测试项目",
  description: "",
  workspacePath: "/tmp/cache-test",
  automationEnabled: false,
  createdAt: "2026-07-14T00:00:00Z",
  updatedAt: "2026-07-14T00:00:00Z",
  version: 1,
} as Project;


const feedbackContext: FeedbackContext = {
  feedback: {
    id: "33333333-3333-4333-8333-333333333333",
    title: "调整 Diff 交互",
    body: "精确反馈说明",
    status: "open",
    createdAt: "2026-07-15T00:00:00Z",
    updatedAt: "2026-07-15T00:00:00Z",
  },
  requirement: {
    id: "22222222-2222-4222-8222-222222222222",
    title: "原需求",
    body: "原始需求说明",
    status: "planned",
    createdAt: "2026-07-14T00:00:00Z",
    updatedAt: "2026-07-15T00:00:00Z",
  },
  association: {
    requirementId: "22222222-2222-4222-8222-222222222222",
    planId: "44444444-4444-4444-8444-444444444444",
    taskId: "55555555-5555-4555-8555-555555555555",
    checkpointId: "66666666-6666-4666-8666-666666666666",
    fileId: "77777777-7777-4777-8777-777777777777",
    diffHunkId: "88888888-8888-4888-8888-888888888888",
    diffLineSide: "new",
    diffLineStart: 11,
    diffLineEnd: 12,
  },
  plan: {
    id: "44444444-4444-4444-8444-444444444444",
    title: "原计划",
    status: "completed",
    markdown: "# 原计划",
  },
  task: {
    id: "55555555-5555-4555-8555-555555555555",
    taskKey: "P006",
    title: "实现反馈追溯",
    status: "succeeded",
    acceptance: "[]",
    acceptanceStatus: "passed",
    acceptanceResult: "{}",
  },
  checkpoint: {
    id: "66666666-6666-4666-8666-666666666666",
    sequence: 3,
    kind: "task_checkpoint",
    changeSummary: "{}",
    gitHead: "abc123",
    createdAt: "2026-07-15T00:00:00Z",
  },
  file: {
    id: "77777777-7777-4777-8777-777777777777",
    path: "frontend/src/App.tsx",
    status: "modified",
    staged: false,
    binary: false,
    additions: 2,
    deletions: 1,
  },
  diff: {
    hunkId: "88888888-8888-4888-8888-888888888888",
    header: "@@ -10,2 +10,3 @@",
    side: "new",
    startLine: 11,
    endLine: 12,
    snippet: "+new line\n+extra line",
  },
  revision: {
    currentStatus: "ready",
    items: [
      {
        id: "99999999-9999-4999-8999-999999999999",
        requirementId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
        requirementTitle: "已有增量修订",
        intakeStatus: "planned",
        planId: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
        planStatus: "ready",
        currentStatus: "ready",
        createdAt: "2026-07-15T01:00:00Z",
      },
    ],
  },
};

function feedbackIntake(): Intake {
  return {
    id: feedbackContext.feedback.id,
    projectId: project.id,
    kind: "feedback",
    parentIntakeId: feedbackContext.requirement.id,
    title: feedbackContext.feedback.title,
    body: feedbackContext.feedback.body,
    status: "open",
    configSnapshot: {},
    createdAt: feedbackContext.feedback.createdAt,
    updatedAt: feedbackContext.feedback.updatedAt,
    version: 1,
  } as Intake;
}

function makeIntake(kind: Intake["kind"], status: Intake["status"]): Intake {
  return {
    id:
      kind === "requirement"
        ? "22222222-2222-4222-8222-222222222222"
        : "33333333-3333-4333-8333-333333333333",
    projectId: project.id,
    kind,
    parentIntakeId:
      kind === "feedback" ? "22222222-2222-4222-8222-222222222222" : undefined,
    title: kind === "feedback" ? "反馈详情" : "需求详情",
    body: kind === "feedback" ? "反馈说明" : "需求说明",
    status,
    configSnapshot: {},
    createdAt: "2026-07-14T00:00:00Z",
    updatedAt: "2026-07-14T00:00:00Z",
    version: 1,
  } as Intake;
}

function renderIntakes(intakes: Intake[]) {
  return render(
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <IntakesView project={project} intakes={intakes} />
    </QueryClientProvider>,
  );
}

function RequirementTabHost() {
  const [visible, setVisible] = useState(true);
  return (
    <QueryClientProvider
      client={
        new QueryClient({ defaultOptions: { queries: { retry: false } } })
      }
    >
      <button onClick={() => setVisible(false)}>切换到计划</button>
      <button onClick={() => setVisible(true)}>返回需求</button>
      <div hidden={!visible}>
        <IntakesView project={project} intakes={[]} />
      </div>
    </QueryClientProvider>
  );
}

describe("IntakesView tab cache", () => {
  afterEach(cleanup);

  beforeEach(() => {
    createIntake.mockReset();
    discussRequirement.mockReset();
    generatePlan.mockReset();
    upload.mockReset();
    createFeedback.mockReset();
    getFeedback.mockReset();
    discussFeedback.mockReset();
    createFeedbackRevision.mockReset();
    generateFeedbackRevisionPlan.mockReset();
    checkpointDiff.mockReset();
    getFeedback.mockResolvedValue(feedbackContext);
    createFeedback.mockResolvedValue({ feedback: feedbackContext.feedback });
    generateFeedbackRevisionPlan.mockResolvedValue({});
  });

  it("keeps unfinished form content and CLI discussion results when the tab is hidden", async () => {
    discussRequirement.mockResolvedValue({
      provider: "codex",
      reply: "建议补充验收标准。",
      ready: false,
      title: "缓存后的标题",
      body: "CLI 整理后的详细说明",
    });
    render(<RequirementTabHost />);

    fireEvent.click(screen.getByRole("button", { name: "创建需求" }));
    fireEvent.change(screen.getByPlaceholderText("希望做出什么改变？"), {
      target: { value: "初始标题" },
    });
    fireEvent.change(
      screen.getByPlaceholderText("请描述背景、约束条件和期望结果…"),
      { target: { value: "初始说明" } },
    );
    fireEvent.click(screen.getByRole("button", { name: "开始讨论" }));
    fireEvent.change(
      screen.getByPlaceholderText("描述你的想法，或回答 CLI 提出的澄清问题…"),
      { target: { value: "请帮我补充细节" } },
    );
    fireEvent.click(screen.getByRole("button", { name: "发送给 CLI" }));

    await waitFor(() =>
      expect(screen.getByText("建议补充验收标准。")).toBeInTheDocument(),
    );
    expect(screen.getByPlaceholderText("希望做出什么改变？")).toHaveValue(
      "缓存后的标题",
    );
    expect(
      screen.getByPlaceholderText("请描述背景、约束条件和期望结果…"),
    ).toHaveValue("CLI 整理后的详细说明");

    fireEvent.click(screen.getByRole("button", { name: "切换到计划" }));
    fireEvent.click(screen.getByRole("button", { name: "返回需求" }));

    expect(screen.getByText("请帮我补充细节")).toBeInTheDocument();
    expect(screen.getByText("建议补充验收标准。")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("希望做出什么改变？")).toHaveValue(
      "缓存后的标题",
    );
    expect(
      screen.getByPlaceholderText("请描述背景、约束条件和期望结果…"),
    ).toHaveValue("CLI 整理后的详细说明");
  });

  it("continues the discussion session and stores it with the created requirement", async () => {
    discussRequirement.mockResolvedValue({
      provider: "codex",
      sessionId: "discussion-session-001",
      reply: "需求已经明确。",
      ready: true,
      title: "会话复用需求",
      body: "需要复用 CLI 上下文。",
    });
    createIntake.mockResolvedValue({
      intake: makeIntake("requirement", "open"),
    });
    renderIntakes([]);

    fireEvent.click(screen.getByRole("button", { name: "创建需求" }));
    fireEvent.click(screen.getByRole("button", { name: "开始讨论" }));
    const composer = screen.getByPlaceholderText(
      "描述你的想法，或回答 CLI 提出的澄清问题…",
    );
    fireEvent.change(composer, { target: { value: "请帮我整理这个需求" } });
    fireEvent.click(screen.getByRole("button", { name: "发送给 CLI" }));

    await waitFor(() => expect(discussRequirement).toHaveBeenCalledTimes(1));
    expect(discussRequirement).toHaveBeenLastCalledWith(project.id, {
      title: "",
      body: "",
      messages: [{ role: "user", content: "请帮我整理这个需求" }],
    });

    fireEvent.change(composer, { target: { value: "补充支持任务链路" } });
    fireEvent.click(screen.getByRole("button", { name: "发送给 CLI" }));
    await waitFor(() => expect(discussRequirement).toHaveBeenCalledTimes(2));
    expect(discussRequirement).toHaveBeenLastCalledWith(project.id, {
      title: "会话复用需求",
      body: "需要复用 CLI 上下文。",
      sessionId: "discussion-session-001",
      sessionProvider: "codex",
      messages: [
        { role: "user", content: "请帮我整理这个需求" },
        { role: "assistant", content: "需求已经明确。" },
        { role: "user", content: "补充支持任务链路" },
      ],
    });

    fireEvent.click(screen.getByRole("button", { name: "保存需求" }));
    await waitFor(() =>
      expect(createIntake).toHaveBeenCalledWith(project.id, {
        kind: "requirement",
        title: "会话复用需求",
        body: "需要复用 CLI 上下文。",
        requirementSessionId: "discussion-session-001",
        requirementSessionProvider: "codex",
      }),
    );
  });

  it("keeps the attachment and requirement actions fixed above the scrollable detail content", () => {
    const requirement = makeIntake("requirement", "open");
    const feedback = makeIntake("feedback", "open");
    const { container } = renderIntakes([requirement, feedback]);

    const detail = container.querySelector(".intake-detail");
    const top = container.querySelector<HTMLElement>(".intake-detail-top")!;
    const scroll = container.querySelector<HTMLElement>(
      ".intake-detail-scroll",
    )!;
    const attachment = within(top)
      .getByText("添加上下文附件")
      .closest<HTMLElement>(".attachment-box");
    const planAction = within(top).getByRole("button", { name: "生成计划" });

    expect(detail).toBeInTheDocument();
    expect(top).toBeInTheDocument();
    expect(scroll).toBeInTheDocument();
    expect(top.parentElement).toBe(detail);
    expect(scroll.parentElement).toBe(detail);

    expect(
      within(top).getByRole("heading", { name: "需求详情" }),
    ).toBeInTheDocument();
    expect(within(top).getByText("计划生成提供方")).toBeInTheDocument();
    expect(planAction).toBeInTheDocument();
    expect(attachment).toBeInTheDocument();
    expect(top).toContainElement(attachment);

    expect(within(scroll).getByText("需求说明")).toBeInTheDocument();
    expect(within(scroll).getByText("关联反馈（1）")).toBeInTheDocument();
    expect(scroll).not.toContainElement(attachment);
    expect(scroll).not.toContainElement(planAction);
  });

  it("uploads the selected attachment from the fixed detail header", async () => {
    const requirement = makeIntake("requirement", "open");
    upload.mockResolvedValue({});
    const { container } = renderIntakes([requirement]);
    const file = new File(["diagnostic output"], "context.log", {
      type: "text/plain",
    });
    const fileInput = container.querySelector<HTMLInputElement>(
      '.intake-detail-top input[type="file"]',
    )!;

    fireEvent.change(fileInput, { target: { files: [file] } });

    await waitFor(() =>
      expect(upload).toHaveBeenCalledWith(requirement.id, file),
    );
  });

  it.each([
    ["requirement", "open", "生成计划"],
    ["requirement", "plan_failed", "生成计划"],
  ] as const)(
    "shows the plan action for a %s intake in %s status",
    (kind, status, actionLabel) => {
      renderIntakes([makeIntake(kind, status)]);

      expect(
        screen.getByRole("button", { name: actionLabel }),
      ).toBeInTheDocument();
      expect(screen.getByText("计划生成提供方")).toBeInTheDocument();
    },
  );

  it.each([
    ["requirement", "planning", "生成计划"],
    ["requirement", "planned", "生成计划"],
    ["requirement", "closed", "生成计划"],
    ["feedback", "open", "生成增量计划"],
    ["feedback", "plan_failed", "生成增量计划"],
    ["feedback", "planning", "生成增量计划"],
    ["feedback", "planned", "生成增量计划"],
    ["feedback", "closed", "生成增量计划"],
  ] as const)(
    "hides the plan action and provider controls for a %s intake in %s status",
    (kind, status, actionLabel) => {
      renderIntakes([makeIntake(kind, status)]);

      expect(
        screen.queryByRole("button", { name: actionLabel }),
      ).not.toBeInTheDocument();
      expect(screen.queryByText("计划生成提供方")).not.toBeInTheDocument();
    },
  );

  it("disables the plan action while the generation request is pending", async () => {
    let resolvePlan: (value: unknown) => void = () => undefined;
    generatePlan.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvePlan = resolve;
        }),
    );
    const intake = makeIntake("requirement", "open");
    renderIntakes([intake]);

    fireEvent.click(screen.getByRole("button", { name: "Codex CLI" }));
    fireEvent.click(screen.getByRole("button", { name: "生成计划" }));

    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "正在加入队列…" }),
      ).toBeDisabled(),
    );
    expect(generatePlan).toHaveBeenCalledWith(intake, "codex");

    resolvePlan({});
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "生成计划" })).toBeEnabled(),
    );
  });

  it("requires a requirement parent and creates feedback with that relationship", async () => {
    const requirement = {
      id: "22222222-2222-4222-8222-222222222222",
      projectId: project.id,
      kind: "requirement",
      title: "已有需求",
      body: "原始需求说明",
      status: "planned",
      configSnapshot: {},
      createdAt: "2026-07-14T00:00:00Z",
      updatedAt: "2026-07-14T00:00:00Z",
      version: 1,
    } as Intake;
    const feedback = {
      id: "33333333-3333-4333-8333-333333333333",
      projectId: project.id,
      kind: "feedback",
      parentIntakeId: requirement.id,
      title: "已有反馈",
      body: "请调整提示文案",
      status: "open",
      configSnapshot: {},
      createdAt: "2026-07-14T00:00:00Z",
      updatedAt: "2026-07-14T00:00:00Z",
      version: 1,
    } as Intake;
    createIntake.mockResolvedValue({ intake: feedback });
    render(
      <QueryClientProvider
        client={
          new QueryClient({ defaultOptions: { queries: { retry: false } } })
        }
      >
        <IntakesView project={project} intakes={[requirement, feedback]} />
      </QueryClientProvider>,
    );

    expect(screen.getByText("关联反馈（1）")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "新建需求" }));
    fireEvent.click(screen.getByRole("button", { name: "反馈" }));

    const submit = screen.getByRole("button", { name: "保存反馈" });
    expect(screen.getByText("关联已有需求")).toBeInTheDocument();
    expect(submit).toBeDisabled();

    fireEvent.change(screen.getByLabelText("目标需求"), {
      target: { value: requirement.id },
    });
    fireEvent.change(screen.getByPlaceholderText("希望调整什么？"), {
      target: { value: "新的反馈" },
    });
    fireEvent.change(
      screen.getByPlaceholderText("请描述背景、约束条件和期望结果…"),
      { target: { value: "需要展示明确的权限处理建议" } },
    );
    expect(submit).toBeEnabled();
    fireEvent.click(submit);

    await waitFor(() =>
      expect(createIntake).toHaveBeenCalledWith(project.id, {
        kind: "feedback",
        parentIntakeId: requirement.id,
        title: "新的反馈",
        body: "需要展示明确的权限处理建议",
      }),
    );
  });

  it("shows the complete navigable trace without exposing full files or Agent logs", async () => {
    const onNavigate = vi.fn();
    render(
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <IntakesView
          project={project}
          intakes={[
            feedbackIntake(),
            {
              id: feedbackContext.revision.items[0].requirementId,
              projectId: project.id,
              kind: "requirement",
              title: feedbackContext.revision.items[0].requirementTitle,
              body: "已有修订说明",
              status: "planned",
              configSnapshot: {},
              createdAt: feedbackContext.revision.items[0].createdAt,
              updatedAt: feedbackContext.revision.items[0].createdAt,
              version: 1,
            } as Intake,
          ]}
          onNavigate={onNavigate}
        />
      </QueryClientProvider>,
    );

    await screen.findByRole("region", { name: "完整追溯链路" });
    const trace = screen.getByRole("region", { name: "完整追溯链路" });
    for (const label of [
      "原需求",
      "原计划",
      "任务",
      "检查点",
      "文件",
      "Diff 行",
      "反馈",
      "修订 Intake",
      "修订 Plan",
    ]) {
      expect(within(trace).getAllByText(label).length).toBeGreaterThan(0);
    }
    expect(within(trace).getAllByText("原需求").length).toBeGreaterThan(0);
    expect(within(trace).getByText("P006 · 实现反馈追溯")).toBeInTheDocument();
    expect(within(trace).getByText("frontend/src/App.tsx")).toBeInTheDocument();
    expect(within(trace).getByText("已有增量修订")).toBeInTheDocument();
    const snippet = screen
      .getByText("已记录的受限 Diff 片段")
      .closest("section")
      ?.querySelector("pre");
    expect(snippet).toHaveTextContent("+new line");
    expect(snippet).toHaveTextContent("+extra line");
    expect(screen.getByText(/不包含完整文件和 Agent 日志/)).toBeInTheDocument();

    fireEvent.click(within(trace).getByRole("button", { name: /原计划/ }));
    expect(onNavigate).toHaveBeenCalledWith({
      kind: "plan",
      id: feedbackContext.plan?.id,
    });
    fireEvent.click(within(trace).getByRole("button", {
        name: /增量计划 · 已有增量修订/,
      }));
    expect(onNavigate).toHaveBeenCalledWith({
      kind: "plan",
      id: feedbackContext.revision.items[0].planId,
    });
  });

  it("shows reverse feedback status and revision state from a requirement", async () => {
    const requirement = makeIntake("requirement", "planned");
    const feedback = feedbackIntake();
    renderIntakes([requirement, feedback]);

    expect(screen.getByText("关联反馈（1）")).toBeInTheDocument();
    await screen.findByText("修订计划就绪");
    expect(screen.getAllByText(feedback.title).length).toBeGreaterThan(0);
    expect(screen.getAllByText("待处理").length).toBeGreaterThan(0);
  });

  it("marks missing associations as invalid and disables navigation and revision", async () => {
    getFeedback.mockResolvedValue({
      ...feedbackContext,
      plan: undefined,
      revision: { currentStatus: "not_started", items: [] },
    });
    renderIntakes([feedbackIntake()]);

    expect(
      await screen.findByText("部分关联对象已失效或无权限"),
    ).toBeInTheDocument();
    const trace = screen.getByRole("region", { name: "完整追溯链路" });
    const invalidPlan = within(trace).getByRole("button", {
      name: /已失效或无权限/,
    });
    expect(invalidPlan).toBeDisabled();
    expect(
      screen.getByPlaceholderText("说明希望如何修正，或回答 CLI 的澄清问题…"),
    ).toBeDisabled();
    expect(
      screen.getByRole("button", { name: "确认并创建增量修订" }),
    ).toBeDisabled();
    expect(screen.getByText(/损坏节点不可跳转/)).toBeInTheDocument();
  });

  it("discusses, confirms, and creates a separate revision before generating a new plan", async () => {
    const revisionIntake = {
      id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
      projectId: project.id,
      kind: "requirement",
      parentIntakeId: feedbackContext.requirement.id,
      title: "新的增量修订",
      body: "只修复所选 Diff 行",
      status: "open",
      configSnapshot: {},
      createdAt: "2026-07-15T02:00:00Z",
      updatedAt: "2026-07-15T02:00:00Z",
      version: 1,
    } as Intake;
    getFeedback.mockResolvedValue({
      ...feedbackContext,
      revision: { currentStatus: "not_started", items: [] },
    });
    discussFeedback.mockResolvedValue({
      provider: "codex",
      reply: "修订范围已经明确。",
      ready: true,
      title: "新的增量修订",
      body: "只修复所选 Diff 行",
    });
    createFeedbackRevision.mockResolvedValue({
      provider: "codex",
      reply: "已创建独立修订。",
      ready: true,
      title: revisionIntake.title,
      body: revisionIntake.body,
      revision: {
        intake: revisionIntake,
        job: null,
        revision: {
          id: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
          feedbackId: feedbackContext.feedback.id,
          projectId: project.id,
          requirementId: feedbackContext.requirement.id,
          revisionIntake,
          createdAt: "2026-07-15T02:00:00Z",
        },
      },
    });
    renderIntakes([feedbackIntake()]);

    expect(await screen.findByText("原计划不会被修改")).toBeInTheDocument();
    expect(
      screen.getByText(/只有为它生成一份新计划，并由用户运行该新计划后/),
    ).toBeInTheDocument();
    fireEvent.change(
      screen.getByPlaceholderText("说明希望如何修正，或回答 CLI 的澄清问题…"),
      { target: { value: "只修复这两行" } },
    );
    fireEvent.click(screen.getByRole("button", { name: "发送并讨论" }));

    expect(await screen.findByText("修订范围已经明确。")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "确认并创建增量修订" }),
    ).toBeEnabled();
    fireEvent.click(
      screen.getByRole("button", { name: "确认并创建增量修订" }),
    );

    await waitFor(() =>
      expect(createFeedbackRevision).toHaveBeenCalledWith(
        project.id,
        feedbackContext.feedback.id,
        expect.objectContaining({
          title: revisionIntake.title,
          body: revisionIntake.body,
          messages: expect.arrayContaining([
            expect.objectContaining({ content: "只修复这两行" }),
            expect.objectContaining({
              content: "确认以上增量修订内容，请创建独立修订。",
            }),
          ]),
        }),
      ),
    );
    expect(await screen.findByText("新的增量修订")).toBeInTheDocument();
    expect(screen.getByText(/下一步先生成独立的新计划/)).toBeInTheDocument();
    fireEvent.click(
      screen.getByRole("button", { name: "生成新的增量计划" }),
    );
    await waitFor(() =>
      expect(generateFeedbackRevisionPlan).toHaveBeenCalledWith(
        revisionIntake,
        undefined,
      ),
    );
  });

});
