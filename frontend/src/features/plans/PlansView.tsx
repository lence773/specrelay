import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "../../api/client";
import type {
  CLIProvider,
  FeedbackReference,
  Plan,
  PlanTask,
  Project,
} from "../../api/types";
import { Check, FileDiff, MessageSquare, Play } from "../../components/Icons";
import { Modal } from "../../components/Modal";
import { Empty, relative, Status } from "../../components/Status";
import {
  CheckpointDiffView,
  FeedbackComposer,
  FeedbackDetailPanel,
  FeedbackReferenceList,
  type FeedbackDraft,
  type FeedbackNavigationTarget,
} from "../feedback/FeedbackViews";

type ProviderChoice = CLIProvider | undefined;

type DiffTarget = {
  checkpointId: string;
  association: Partial<FeedbackDraft>;
};

const providerOptions: {
  value: ProviderChoice;
  label: string;
  description: string;
}[] = [
  { value: undefined, label: "项目默认", description: "由服务端读取当前设置" },
  { value: "codex", label: "Codex CLI", description: "覆盖本次执行" },
  { value: "claude", label: "Claude CLI", description: "覆盖本次执行" },
];

function ExecutionProviderSelector({
  label,
  value,
  onChange,
  disabled = false,
}: {
  label: string;
  value: ProviderChoice;
  onChange: (provider: ProviderChoice) => void;
  disabled?: boolean;
}) {
  return (
    <fieldset className="cli-provider-selector" disabled={disabled}>
      <legend>{label}</legend>
      <div className="cli-provider-options" role="group" aria-label={label}>
        {providerOptions.map((option) => (
          <button
            type="button"
            aria-label={`${label}：${option.label}`}
            aria-pressed={value === option.value}
            className={value === option.value ? "active" : ""}
            key={option.value ?? "default"}
            onClick={() => onChange(option.value)}
          >
            <strong>{option.label}</strong>
            <span>{option.description}</span>
          </button>
        ))}
      </div>
    </fieldset>
  );
}

function isOrdinaryTaskRunnable(tasks: PlanTask[], index: number) {
  const task = tasks[index];
  return (
    task.title !== "Final validation" &&
    ["pending", "failed", "cancelled"].includes(task.status) &&
    tasks.slice(0, index).every((previous) => previous.status === "succeeded")
  );
}

function taskCheckpointId(task: PlanTask) {
  const taskRecord = task as unknown as Record<string, unknown>;
  for (const key of ["checkpointId", "latestCheckpointId", "snapshotId"])
    if (typeof taskRecord[key] === "string") return taskRecord[key] as string;
  const result = task.acceptanceResult;
  if (result && typeof result === "object")
    for (const key of ["checkpointId", "latestCheckpointId", "snapshotId"])
      if (typeof result[key] === "string") return result[key] as string;
  return undefined;
}

function TaskFeedbackBlock({
  project,
  plan,
  task,
  onCreate,
  onOpenFeedback,
  onOpenDiff,
}: {
  project: Project;
  plan: Plan;
  task: PlanTask;
  onCreate: (draft: FeedbackDraft) => void;
  onOpenFeedback: (id: string) => void;
  onOpenDiff: (target: DiffTarget) => void;
}) {
  const detail = useQuery({
    queryKey: ["task", task.id],
    queryFn: () => api.task(task.id),
    enabled: typeof api.task === "function",
    retry: false,
  });
  const references = detail.data?.feedback ?? [];
  const detailedTask = detail.data?.task ?? task;
  const checkpointId = taskCheckpointId(detailedTask);
  const requirementId = references[0]?.requirementId ?? plan.intakeId;
  return (
    <section
      className="task-feedback-block"
      aria-label={`${task.taskKey} 反馈`}
    >
      <div className="task-feedback-heading">
        <div>
          <MessageSquare />
          <strong>关联反馈（{references.length}）</strong>
          {detail.isError && <em>关联状态不可用或无权限</em>}
        </div>
        <div>
          {checkpointId && (
            <button
              type="button"
              className="button ghost small"
              onClick={() =>
                onOpenDiff({
                  checkpointId,
                  association: {
                    requirementId,
                    planId: plan.id,
                    planTitle: plan.title,
                    taskId: detailedTask.id,
                    taskKey: detailedTask.taskKey,
                    taskTitle: detailedTask.title,
                  },
                })
              }
            >
              <FileDiff /> 查看变更与 Diff
            </button>
          )}
          <button
            type="button"
            className="button secondary small"
            onClick={() =>
              onCreate({
                requirementId,
                planId: plan.id,
                planTitle: plan.title,
                taskId: detailedTask.id,
                taskKey: detailedTask.taskKey,
                taskTitle: detailedTask.title,
              })
            }
          >
            创建反馈
          </button>
        </div>
      </div>
      <FeedbackReferenceList items={references} onOpen={onOpenFeedback} />
    </section>
  );
}

export function PlansView({
  project,
  plans,
  focus,
  onNavigate,
}: {
  project: Project;
  plans: Plan[];
  focus?: FeedbackNavigationTarget;
  onNavigate?: (target: FeedbackNavigationTarget) => void;
}) {
  const [selected, setSelected] = useState(plans[0]?.id);
  const [planProvider, setPlanProvider] = useState<ProviderChoice>();
  const [taskProviders, setTaskProviders] = useState<
    Record<string, ProviderChoice>
  >({});
  const [feedbackDraft, setFeedbackDraft] = useState<FeedbackDraft>();
  const [diffTarget, setDiffTarget] = useState<DiffTarget>();
  const [localFeedbackId, setLocalFeedbackId] = useState<string>();
  const queryClient = useQueryClient();
  const detail = useQuery({
    queryKey: ["plan", selected],
    queryFn: () => api.plan(selected!),
    enabled: !!selected,
  });
  const refreshPlan = (planId: string) =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: ["plans", project.id] }),
      queryClient.invalidateQueries({ queryKey: ["plan", planId] }),
    ]);
  const runPlan = useMutation({
    mutationFn: ({ plan, provider }: { plan: Plan; provider?: CLIProvider }) =>
      api.runPlan(plan, provider),
    onSuccess: async (_, variables) => {
      setPlanProvider(undefined);
      await refreshPlan(variables.plan.id);
    },
    onError: async (_, variables) => {
      await refreshPlan(variables.plan.id);
    },
  });
  const runTask = useMutation({
    mutationFn: ({
      task,
      provider,
    }: {
      task: PlanTask;
      provider?: CLIProvider;
    }) => api.runTask(task, provider),
    onSuccess: async (_, variables) => {
      setTaskProviders((current) => {
        const next = { ...current };
        delete next[variables.task.id];
        return next;
      });
      await refreshPlan(variables.task.planId);
    },
    onError: async (_, variables) => {
      await refreshPlan(variables.task.planId);
    },
  });

  const focusedPlanId =
    focus?.kind === "plan"
      ? focus.id
      : focus?.kind === "task"
        ? focus.planId
        : focus?.kind === "checkpoint"
          ? focus.planId
          : undefined;
  useEffect(() => {
    if (
      selected &&
      (plans.some((plan) => plan.id === selected) || selected === focusedPlanId)
    )
      return;
    setSelected(plans[0]?.id);
  }, [focusedPlanId, plans, selected]);
  useEffect(() => {
    setPlanProvider(undefined);
    setTaskProviders({});
  }, [selected]);
  useEffect(() => {
    if (!focus) return;
    if (focus.kind === "plan") setSelected(focus.id);
    if (focus.kind === "task") setSelected(focus.planId);
    if (focus.kind === "checkpoint") {
      if (focus.planId) setSelected(focus.planId);
      setDiffTarget({
        checkpointId: focus.id,
        association: {
          requirementId: focus.requirementId,
          planId: focus.planId,
          taskId: focus.taskId,
        },
      });
    }
  }, [focus]);
  useEffect(() => {
    if (
      focus?.kind !== "task" ||
      detail.data?.plan.id !== focus.planId
    )
      return;
    const target = Array.from(
      document.querySelectorAll<HTMLElement>("[data-task-id]"),
    ).find((element) => element.dataset.taskId === focus.id);
    target?.scrollIntoView?.({ block: "center" });
    target?.focus({ preventScroll: true });
  }, [detail.data, focus]);

  const selectPlan = (planId: string) => setSelected(planId);
  const executionPending = runPlan.isPending || runTask.isPending;
  const shownPlan = detail.data?.plan;
  const planFeedback: FeedbackReference[] = detail.data?.feedback ?? [];
  const planRequirementId =
    planFeedback[0]?.requirementId ?? shownPlan?.intakeId;
  const canRunPlan =
    shownPlan && ["ready", "blocked"].includes(shownPlan.status);
  const completedTaskCount =
    detail.data?.tasks.filter((task) => task.status === "succeeded").length ??
    0;
  const planError =
    runPlan.isError && runPlan.variables?.plan.id === shownPlan?.id
      ? runPlan.error
      : undefined;
  const openFeedback = (id: string) => {
    if (onNavigate) onNavigate({ kind: "feedback", id });
    else setLocalFeedbackId(id);
  };

  return (
    <div className="split-page plans-page split-scroll-page">
      <section className="collection split-scroll-collection">
        <header>
          <div>
            <span className="eyebrow">执行中心</span>
            <h1>计划</h1>
          </div>
        </header>
        {plans.length === 0 ? (
          <Empty
            title="暂无结构化计划"
            body="计划会根据智能体生成的 PlanSpec JSON 确定性渲染。"
          />
        ) : (
          <div className="plan-list app-scroll-region">
            {plans.map((plan) => (
              <article
                key={plan.id}
                className={`plan-list-card${plan.id === selected ? " selected" : ""}`}
              >
                <button
                  type="button"
                  className="plan-select"
                  onClick={() => selectPlan(plan.id)}
                >
                  <div>
                    <Status value={plan.status} />
                    <small>{relative(plan.updatedAt)}</small>
                  </div>
                  <strong>{plan.title}</strong>
                  <span>计划版本 {plan.version}</span>
                </button>
                <button
                  type="button"
                  className="plan-feedback-entry"
                  aria-label={`为计划 ${plan.title} 创建反馈`}
                  onClick={() =>
                    setFeedbackDraft({
                      requirementId: plan.intakeId,
                      planId: plan.id,
                      planTitle: plan.title,
                    })
                  }
                >
                  <MessageSquare /> 创建反馈
                </button>
              </article>
            ))}
          </div>
        )}
      </section>
      <section
        className={`plan-detail split-scroll-detail${detail.data ? "" : " app-scroll-region"}`}
        style={
          detail.data
            ? { display: "flex", flexDirection: "column", overflow: "hidden" }
            : undefined
        }
      >
        {detail.isLoading ? (
          <div className="loading">正在加载计划…</div>
        ) : detail.data ? (
          <>
            <header style={{ flex: "0 0 auto" }}>
              <div>
                <span className="eyebrow">结构化交付计划</span>
                <h2>{detail.data.plan.title}</h2>
                <div className="detail-meta">
                  <Status value={detail.data.plan.status} />
                  <span>
                    已完成 {completedTaskCount} / {detail.data.tasks.length}
                  </span>
                  <span>关联反馈 {planFeedback.length}</span>
                </div>
              </div>
              <button
                type="button"
                className="button secondary small"
                onClick={() =>
                  planRequirementId &&
                  setFeedbackDraft({
                    requirementId: planRequirementId,
                    planId: detail.data.plan.id,
                    planTitle: detail.data.plan.title,
                  })
                }
                disabled={!planRequirementId}
              >
                <MessageSquare /> 创建反馈
              </button>
            </header>
            {canRunPlan && (
              <section
                className="plan-execution-panel"
                aria-label="运行整份计划"
                style={{ flex: "0 0 auto" }}
              >
                <div className="execution-panel-main">
                  <ExecutionProviderSelector
                    label="整份计划执行提供方"
                    value={planProvider}
                    onChange={setPlanProvider}
                    disabled={executionPending}
                  />
                  <p>
                    显式选择会应用于本次计划排入的所有普通任务及其自动重试；选择“项目默认”时不会固化提供方，也不会影响其他计划。
                  </p>
                </div>
                <button
                  className="button primary plan-run-button"
                  onClick={() =>
                    runPlan.mutate({
                      plan: detail.data.plan,
                      provider: planProvider,
                    })
                  }
                  disabled={executionPending}
                >
                  <Play /> {runPlan.isPending ? "正在加入队列…" : "运行计划"}
                </button>
                {planError && (
                  <div className="form-error execution-error" role="alert">
                    运行计划失败：{planError.message}
                    。状态已刷新，可调整提供方后重试。
                  </div>
                )}
              </section>
            )}
            <div className="progress" style={{ flex: "0 0 auto" }}>
              <i
                style={{
                  width: `${detail.data.tasks.length ? (completedTaskCount / detail.data.tasks.length) * 100 : 0}%`,
                }}
              />
            </div>
            <div
              className="plan-detail-scroll app-scroll-region"
              aria-label="计划内容"
              style={{
                flex: "1 1 auto",
                minHeight: 0,
                overflowY: "auto",
                overscrollBehaviorY: "contain",
                paddingRight: 10,
                scrollbarGutter: "stable",
              }}
            >
              <section
                className="plan-feedback-summary"
                aria-label="计划关联反馈"
              >
                <div>
                  <strong>计划关联反馈（{planFeedback.length}）</strong>
                  <span>反馈状态与后续修订状态</span>
                </div>
                <FeedbackReferenceList
                  items={planFeedback}
                  onOpen={openFeedback}
                />
              </section>
              <div className="task-track">
                {detail.data.tasks.map((task, index) => {
                  const isValidation = task.title === "Final validation";
                  const canRunTask = isOrdinaryTaskRunnable(
                    detail.data.tasks,
                    index,
                  );
                  const provider = taskProviders[task.id];
                  const taskPending =
                    runTask.isPending && runTask.variables?.task.id === task.id;
                  const taskError =
                    runTask.isError && runTask.variables?.task.id === task.id
                      ? runTask.error
                      : undefined;
                  return (
                    <article
                      key={task.id}
                      data-task-id={task.id}
                      className={`task task-${task.status}${isValidation ? " task-validation" : ""}${focus?.kind === "task" && focus.id === task.id ? " focused" : ""}`}
                      tabIndex={-1}
                    >
                      <div className="task-marker">
                        {task.status === "succeeded" ? (
                          <Check />
                        ) : (
                          task.position
                        )}
                      </div>
                      <div>
                        <div className="task-heading">
                          <strong>
                            {task.taskKey} · {task.title}
                          </strong>
                          <Status value={task.status} />
                        </div>
                        <div className="scope-list">
                          {task.scope.map((path) => (
                            <code key={path}>{path}</code>
                          ))}
                        </div>
                        <ul>
                          {task.acceptance.map((item) => (
                            <li key={item}>{item}</li>
                          ))}
                        </ul>
                        <TaskFeedbackBlock
                          project={project}
                          plan={detail.data.plan}
                          task={task}
                          onCreate={setFeedbackDraft}
                          onOpenFeedback={openFeedback}
                          onOpenDiff={setDiffTarget}
                        />
                        {canRunTask && (
                          <div className="task-execution-panel">
                            <ExecutionProviderSelector
                              label={`${task.taskKey} 本次执行提供方`}
                              value={provider}
                              onChange={(value) =>
                                setTaskProviders((current) => ({
                                  ...current,
                                  [task.id]: value,
                                }))
                              }
                              disabled={executionPending}
                            />
                            <div className="task-execution-actions">
                              <span>
                                只影响这次手动
                                {task.status === "failed" ? "重试" : "运行"}
                                ，不会继承计划或其他任务的选择。
                              </span>
                              <button
                                className="button small"
                                onClick={() =>
                                  runTask.mutate({ task, provider })
                                }
                                disabled={executionPending}
                              >
                                <Play />{" "}
                                {taskPending
                                  ? "正在加入队列…"
                                  : task.status === "failed"
                                    ? "重试任务"
                                    : "运行任务"}
                              </button>
                            </div>
                            {taskError && (
                              <div
                                className="form-error execution-error"
                                role="alert"
                              >
                                {task.status === "failed" ? "重试" : "运行"}
                                任务失败：{taskError.message}
                                。状态已刷新，可重新选择后再试。
                              </div>
                            )}
                          </div>
                        )}
                        {isValidation && (
                          <div className="task-validation-note">
                            最终验证仅使用项目配置的验证命令，不通过 Codex CLI
                            或 Claude CLI 手动执行。
                          </div>
                        )}
                      </div>
                    </article>
                  );
                })}
              </div>
              <details className="markdown-panel">
                <summary>查看渲染后的计划文档</summary>
                <div className="markdown">
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>
                    {detail.data.plan.markdown}
                  </ReactMarkdown>
                </div>
              </details>
            </div>
          </>
        ) : detail.error ? (
          <div className="broken-relation" role="alert">
            <strong>计划已不存在或无权限</strong>
            <p>无法读取计划详情，关联链接和反馈操作已禁用。</p>
          </div>
        ) : (
          <Empty title="请选择一个计划" body="查看任务、验收标准和执行状态。" />
        )}
      </section>
      {feedbackDraft && (
        <FeedbackComposer
          project={project}
          draft={feedbackDraft}
          onClose={() => setFeedbackDraft(undefined)}
          onCreated={openFeedback}
        />
      )}
      {diffTarget && (
        <Modal
          title="变更与 Diff"
          onClose={() => setDiffTarget(undefined)}
          wide
          className="diff-modal"
        >
          <CheckpointDiffView
            project={project}
            checkpointId={diffTarget.checkpointId}
            association={diffTarget.association}
            onOpenFeedback={openFeedback}
          />
        </Modal>
      )}
      {localFeedbackId && (
        <Modal
          title="反馈详情"
          onClose={() => setLocalFeedbackId(undefined)}
          wide
          className="feedback-detail-modal"
        >
          <FeedbackDetailPanel project={project} feedbackId={localFeedbackId} />
        </Modal>
      )}
    </div>
  );
}
