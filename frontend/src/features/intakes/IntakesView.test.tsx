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
import type { Intake, Project } from "../../api/types";

const { createIntake, discussRequirement, generatePlan, upload } = vi.hoisted(
  () => ({
    createIntake: vi.fn(),
    discussRequirement: vi.fn(),
    generatePlan: vi.fn(),
    upload: vi.fn(),
  }),
);
vi.mock("../../api/client", () => ({
  api: { createIntake, discussRequirement, generatePlan, upload },
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
    ["feedback", "open", "生成增量计划"],
    ["feedback", "plan_failed", "生成增量计划"],
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
});
