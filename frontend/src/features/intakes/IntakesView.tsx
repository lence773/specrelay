import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  CLIProvider,
  Intake,
  IntakeInput,
  Project,
  RequirementDiscussionMessage,
} from "../../api/types";
import { Plus, Upload } from "../../components/Icons";
import { Empty, kindLabel, relative, Status } from "../../components/Status";

type ProviderChoice = CLIProvider | undefined;
type DiscussionMessage = RequirementDiscussionMessage & {
  provider?: CLIProvider;
};
type IntakeFilter = "all" | "requirement" | "feedback";

const providerOptions: {
  value: ProviderChoice;
  label: string;
  description: string;
}[] = [
  { value: undefined, label: "项目默认", description: "发送时不覆盖项目设置" },
  { value: "codex", label: "Codex CLI", description: "仅用于这次操作" },
  { value: "claude", label: "Claude CLI", description: "仅用于这次操作" },
];

function providerLabel(provider: ProviderChoice) {
  return provider === "claude"
    ? "Claude CLI"
    : provider === "codex"
      ? "Codex CLI"
      : "项目默认 CLI";
}

function canGeneratePlan(status: Intake["status"]) {
  return status === "open" || status === "plan_failed";
}

function ProviderSelector({
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
            aria-label={option.label}
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

export function IntakesView({
  project,
  intakes,
}: {
  project: Project;
  intakes: Intake[];
}) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | undefined>(intakes[0]?.id);
  const [creating, setCreating] = useState(false);
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [kind, setKind] = useState<"requirement" | "feedback">("requirement");
  const [feedbackParentID, setFeedbackParentID] = useState("");
  const [filter, setFilter] = useState<IntakeFilter>("all");
  const [discussionOpen, setDiscussionOpen] = useState(false);
  const [discussionMessages, setDiscussionMessages] = useState<
    DiscussionMessage[]
  >([]);
  const [discussionInput, setDiscussionInput] = useState("");
  const [discussionProvider, setDiscussionProvider] =
    useState<ProviderChoice>();
  const [newPlanProvider, setNewPlanProvider] = useState<ProviderChoice>();
  const [existingPlanProvider, setExistingPlanProvider] =
    useState<ProviderChoice>();
  const [discussionReady, setDiscussionReady] = useState(false);
  const discussionSession = useRef(0);
  const fileRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (selected && intakes.some((intake) => intake.id === selected)) return;
    setSelected(intakes[0]?.id);
  }, [intakes, selected]);
  useEffect(() => setExistingPlanProvider(undefined), [selected]);

  const item = intakes.find((i) => i.id === selected);
  const planGenerationAvailable = item ? canGeneratePlan(item.status) : false;
  const requirements = intakes.filter(
    (intake) => intake.kind === "requirement",
  );
  const visibleIntakes =
    filter === "all"
      ? intakes
      : intakes.filter((intake) => intake.kind === filter);
  const feedbackParent =
    item?.kind === "feedback" && item.parentIntakeId
      ? intakes.find((intake) => intake.id === item.parentIntakeId)
      : undefined;
  const relatedFeedback =
    item?.kind === "requirement"
      ? intakes.filter(
          (intake) =>
            intake.kind === "feedback" && intake.parentIntakeId === item.id,
        )
      : [];
  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ["intakes", project.id] });
  const clearDiscussion = () => {
    discussionSession.current += 1;
    setDiscussionMessages([]);
    setDiscussionInput("");
    setDiscussionProvider(undefined);
    setDiscussionReady(false);
    setDiscussionOpen(false);
  };
  const clearCreateDraft = () => {
    setTitle("");
    setBody("");
    setKind("requirement");
    setFeedbackParentID("");
    setNewPlanProvider(undefined);
    clearDiscussion();
  };

  const create = useMutation({
    mutationFn: ({ provider }: { provider?: CLIProvider }) => {
      const input: IntakeInput =
        kind === "feedback"
          ? { kind, title, body, parentIntakeId: feedbackParentID }
          : { kind, title, body };
      return api.createIntake(
        project.id,
        provider === undefined ? input : { ...input, provider },
      );
    },
    onSuccess: (result) => {
      invalidate();
      setSelected(result.intake.id);
      setCreating(false);
      clearCreateDraft();
    },
  });
  const discuss = useMutation({
    mutationFn: ({
      messages,
      provider,
    }: {
      messages: DiscussionMessage[];
      provider?: CLIProvider;
      session: number;
    }) => {
      const input = {
        title,
        body,
        messages: messages.map(({ role, content }) => ({ role, content })),
      };
      return api.discussRequirement(
        project.id,
        provider === undefined ? input : { ...input, provider },
      );
    },
    onSuccess: (result, { messages, session }) => {
      if (session !== discussionSession.current) return;
      setDiscussionMessages([
        ...messages,
        { role: "assistant", content: result.reply, provider: result.provider },
      ]);
      setDiscussionReady(result.ready);
      if (result.title) setTitle(result.title);
      if (result.body) setBody(result.body);
    },
  });
  const generate = useMutation({
    mutationFn: ({
      intake,
      provider,
    }: {
      intake: Intake;
      provider?: CLIProvider;
    }) => api.generatePlan(intake, provider),
    onSuccess: invalidate,
  });
  const upload = useMutation({
    mutationFn: (file: File) => api.upload(item!.id, file),
  });

  const beginCreate = () => {
    if (create.isPending || discuss.isPending) return;
    clearCreateDraft();
    create.reset();
    discuss.reset();
    setCreating(true);
  };
  const closeCreate = () => {
    if (create.isPending) return;
    setCreating(false);
    clearCreateDraft();
    create.reset();
    discuss.reset();
  };
  const selectIntake = (id: string) => {
    if (create.isPending) return;
    setSelected(id);
    setExistingPlanProvider(undefined);
    generate.reset();
    closeCreate();
  };
  const sendDiscussionMessage = () => {
    const content = discussionInput.trim();
    if (!content || discuss.isPending) return;
    const messages: DiscussionMessage[] = [
      ...discussionMessages,
      { role: "user", content },
    ];
    setDiscussionMessages(messages);
    setDiscussionInput("");
    setDiscussionReady(false);
    discuss.mutate({
      messages,
      provider: discussionProvider,
      session: discussionSession.current,
    });
  };
  const submitCreate = () => {
    if (
      create.isPending ||
      discuss.isPending ||
      (kind === "feedback" && !feedbackParentID)
    )
      return;
    create.mutate({
      provider: project.automationEnabled ? newPlanProvider : undefined,
    });
  };
  const queuePlan = () => {
    if (!item || !planGenerationAvailable || generate.isPending) return;
    generate.mutate({ intake: item, provider: existingPlanProvider });
  };

  return (
    <div className="split-page split-scroll-page">
      <section className="collection split-scroll-collection">
        <header>
          <div>
            <span className="eyebrow">输入队列</span>
            <h1>需求与反馈</h1>
          </div>
          <button
            className="button primary small"
            onClick={beginCreate}
            disabled={create.isPending || discuss.isPending}
          >
            <Plus /> 新建需求
          </button>
        </header>
        <div className="filter-row">
          <button
            className={`chip ${filter === "all" ? "active" : ""}`}
            onClick={() => setFilter("all")}
          >
            全部 <b>{intakes.length}</b>
          </button>
          <button
            className={`chip ${filter === "requirement" ? "active" : ""}`}
            onClick={() => setFilter("requirement")}
          >
            需求 <b>{requirements.length}</b>
          </button>
          <button
            className={`chip ${filter === "feedback" ? "active" : ""}`}
            onClick={() => setFilter("feedback")}
          >
            反馈 <b>{intakes.length - requirements.length}</b>
          </button>
        </div>
        {intakes.length === 0 ? (
          <Empty
            title="输入队列为空"
            body="先创建需求；反馈会关联到已有需求，并生成增量修正计划。"
            action={
              <button
                className="button primary"
                onClick={beginCreate}
                disabled={create.isPending || discuss.isPending}
              >
                创建需求
              </button>
            }
          />
        ) : visibleIntakes.length === 0 ? (
          <Empty
            title="没有匹配的输入"
            body="切换筛选条件，或创建一条新的输入。"
          />
        ) : (
          <div className="intake-list app-scroll-region">
            {visibleIntakes.map((intake) => {
              const parent =
                intake.kind === "feedback" && intake.parentIntakeId
                  ? intakes.find(
                      (candidate) => candidate.id === intake.parentIntakeId,
                    )
                  : undefined;
              return (
                <button
                  key={intake.id}
                  className={intake.id === selected ? "selected" : ""}
                  onClick={() => selectIntake(intake.id)}
                  disabled={create.isPending}
                >
                  <div>
                    <span className={`kind kind-${intake.kind}`}>
                      {kindLabel(intake.kind)}
                    </span>
                    <Status value={intake.status} />
                  </div>
                  <strong>{intake.title}</strong>
                  <p>{intake.body || "暂无说明"}</p>
                  {parent && (
                    <em className="intake-parent">关联：{parent.title}</em>
                  )}
                  <small>{relative(intake.updatedAt)}</small>
                </button>
              );
            })}
          </div>
        )}
      </section>
      <section
        className={`editor split-scroll-detail${creating || !item ? " app-scroll-region" : ""}`}
        style={
          !creating && item
            ? {
                display: "flex",
                flexDirection: "column",
                overflow: "hidden",
              }
            : undefined
        }
      >
        {creating ? (
          <form
            onSubmit={(event) => {
              event.preventDefault();
              submitCreate();
            }}
          >
            <header>
              <div>
                <span className="eyebrow">新建输入</span>
                <h2>定义工作内容</h2>
              </div>
              <div className="segmented">
                <button
                  type="button"
                  className={kind === "requirement" ? "active" : ""}
                  onClick={() => setKind("requirement")}
                  disabled={create.isPending}
                >
                  需求
                </button>
                <button
                  type="button"
                  className={kind === "feedback" ? "active" : ""}
                  onClick={() => setKind("feedback")}
                  disabled={create.isPending}
                >
                  反馈
                </button>
              </div>
            </header>

            {kind === "requirement" && (
              <section
                className={`discussion-panel ${discussionOpen ? "open" : ""}`}
              >
                <div className="discussion-header">
                  <div>
                    <div className="discussion-title">
                      <strong>与本地 CLI 讨论需求</strong>
                      <span className="discussion-badge">只读分析</span>
                    </div>
                    <p>
                      按轮次选择项目默认、Codex 或
                      Claude，读取本地代码上下文并帮助澄清需求，不会修改项目文件。
                    </p>
                  </div>
                  <button
                    type="button"
                    className="button secondary small"
                    onClick={() => setDiscussionOpen((value) => !value)}
                    disabled={discuss.isPending}
                  >
                    {discussionOpen ? "收起讨论" : "开始讨论"}
                  </button>
                </div>
                {discussionOpen && (
                  <div className="discussion-content">
                    <div
                      className="discussion-messages app-scroll-region"
                      aria-live="polite"
                    >
                      {discussionMessages.length === 0 ? (
                        <div className="discussion-empty">
                          <strong>先说说你想实现什么</strong>
                          <span>
                            CLI
                            会结合当前项目提出澄清问题，并逐步整理标题、需求说明和验收标准。
                          </span>
                        </div>
                      ) : (
                        discussionMessages.map((message, index) => (
                          <div
                            className={`discussion-message ${message.role}`}
                            key={`${message.role}-${index}`}
                          >
                            <div className="discussion-message-meta">
                              {message.role === "user"
                                ? "你"
                                : providerLabel(message.provider)}
                            </div>
                            <div>{message.content}</div>
                          </div>
                        ))
                      )}
                      {discuss.isPending && (
                        <div className="discussion-message assistant pending">
                          <div className="discussion-message-meta">
                            {providerLabel(discuss.variables?.provider)}
                          </div>
                          <div>
                            <span className="discussion-pulse" />
                            正在只读分析本地项目并整理需求，请稍候…
                          </div>
                        </div>
                      )}
                    </div>
                    {discussionReady && (
                      <div className="discussion-ready">
                        <strong>需求已经足够明确</strong>
                        <span>
                          CLI
                          整理的标题和说明已写入下方表单。确认内容后即可创建需求
                          {project.automationEnabled ? "并自动生成计划" : "。"}
                        </span>
                      </div>
                    )}
                    {discuss.error && (
                      <div className="form-error">
                        CLI 讨论失败：{discuss.error.message}
                      </div>
                    )}
                    <div className="discussion-composer">
                      <ProviderSelector
                        label="本轮讨论提供方"
                        value={discussionProvider}
                        onChange={setDiscussionProvider}
                        disabled={discuss.isPending}
                      />
                      <textarea
                        value={discussionInput}
                        onChange={(event) =>
                          setDiscussionInput(event.target.value)
                        }
                        onKeyDown={(event) => {
                          if (
                            (event.ctrlKey || event.metaKey) &&
                            event.key === "Enter"
                          ) {
                            event.preventDefault();
                            sendDiscussionMessage();
                          }
                        }}
                        placeholder="描述你的想法，或回答 CLI 提出的澄清问题…"
                        rows={3}
                        disabled={discuss.isPending}
                      />
                      <div className="discussion-actions">
                        <span>按 Ctrl/⌘ + Enter 发送</span>
                        <button
                          type="button"
                          className="button primary"
                          onClick={sendDiscussionMessage}
                          disabled={
                            !discussionInput.trim() || discuss.isPending
                          }
                        >
                          {discuss.isPending ? "分析中…" : "发送给 CLI"}
                        </button>
                      </div>
                    </div>
                  </div>
                )}
              </section>
            )}

            {kind === "feedback" && (
              <section className="feedback-link-panel">
                <strong>关联已有需求</strong>
                <p>
                  反馈会连同原需求及其既有计划一起交给
                  CLI，生成只覆盖本次变化的增量修正计划。
                </p>
                <label>
                  <span>目标需求</span>
                  <select
                    value={feedbackParentID}
                    onChange={(event) =>
                      setFeedbackParentID(event.target.value)
                    }
                    required
                    disabled={create.isPending}
                  >
                    <option value="">请选择需要反馈的需求…</option>
                    {requirements.map((requirement) => (
                      <option key={requirement.id} value={requirement.id}>
                        {requirement.title}
                      </option>
                    ))}
                  </select>
                </label>
                {requirements.length === 0 && (
                  <div className="form-error">
                    请先创建一条需求，才能提交关联反馈。
                  </div>
                )}
              </section>
            )}
            <label>
              <span>标题</span>
              <input
                autoFocus={!discussionOpen}
                value={title}
                onChange={(event) => setTitle(event.target.value)}
                placeholder={
                  kind === "feedback" ? "希望调整什么？" : "希望做出什么改变？"
                }
                required
                disabled={create.isPending}
              />
            </label>
            <label className="grow">
              <span>详细说明</span>
              <textarea
                value={body}
                onChange={(event) => setBody(event.target.value)}
                placeholder="请描述背景、约束条件和期望结果…"
                required
                disabled={create.isPending}
              />
            </label>
            {project.automationEnabled && (
              <div className="plan-provider-block">
                <ProviderSelector
                  label="计划生成提供方"
                  value={newPlanProvider}
                  onChange={setNewPlanProvider}
                  disabled={create.isPending || discuss.isPending}
                />
                <p>
                  {kind === "feedback"
                    ? "会基于关联需求与其既有计划生成增量修正计划；"
                    : "仅覆盖本次自动生成计划；选择“项目默认”时，请求不会携带提供方覆盖值。"}
                </p>
              </div>
            )}
            {create.error && (
              <div className="form-error">提交失败：{create.error.message}</div>
            )}
            <footer>
              <button
                type="button"
                className="button ghost"
                onClick={closeCreate}
                disabled={create.isPending}
              >
                取消
              </button>
              <button
                className="button primary"
                disabled={
                  create.isPending ||
                  discuss.isPending ||
                  (kind === "feedback" && !feedbackParentID)
                }
              >
                {create.isPending
                  ? "正在提交…"
                  : project.automationEnabled
                    ? kind === "feedback"
                      ? "提交并生成增量计划"
                      : "提交并生成计划"
                    : kind === "feedback"
                      ? "保存反馈"
                      : "保存需求"}
              </button>
            </footer>
          </form>
        ) : item ? (
          <article
            className="intake-detail"
            style={{
              flex: "1 1 auto",
              height: "100%",
              minHeight: 0,
              width: "100%",
            }}
          >
            <div className="intake-detail-top" style={{ flex: "0 0 auto" }}>
              <header
                style={{
                  display: "flex",
                  alignItems: "flex-start",
                  justifyContent: "space-between",
                  marginBottom: 28,
                }}
              >
                <div>
                  <div className="detail-meta">
                    <span className={`kind kind-${item.kind}`}>
                      {kindLabel(item.kind)}
                    </span>
                    <Status value={item.status} />
                  </div>
                  <h2>{item.title}</h2>
                  <small>
                    创建于 {new Date(item.createdAt).toLocaleString("zh-CN")}
                  </small>
                </div>
              </header>
              <div
                className="attachment-box"
                onClick={() => fileRef.current?.click()}
              >
                <Upload />
                <div>
                  <strong>添加上下文附件</strong>
                  <span>支持图片、日志和参考文件，单个文件最大 50 MiB</span>
                </div>
                <input
                  ref={fileRef}
                  type="file"
                  hidden
                  onChange={(event) => {
                    const file = event.target.files?.[0];
                    if (file) upload.mutate(file);
                  }}
                />
              </div>
              {upload.error && (
                <div className="form-error">
                  上传失败：{upload.error.message}
                </div>
              )}
              {planGenerationAvailable && (
                <>
                  <div
                    className="plan-provider-block existing-plan-provider"
                    style={{ marginTop: 0 }}
                  >
                    <ProviderSelector
                      label="计划生成提供方"
                      value={existingPlanProvider}
                      onChange={setExistingPlanProvider}
                      disabled={generate.isPending}
                    />
                    <p>
                      {item.kind === "feedback"
                        ? "会基于关联需求与其既有计划生成增量修正计划；"
                        : "只影响下一次入队的计划生成，不会修改项目默认设置。"}
                    </p>
                  </div>
                  {generate.error && (
                    <div className="form-error">
                      生成计划失败：{generate.error.message}
                    </div>
                  )}
                  <div
                    className="intake-detail-actions"
                    style={{
                      display: "flex",
                      justifyContent: "flex-end",
                      marginTop: 15,
                    }}
                  >
                    <button
                      className="button secondary"
                      onClick={queuePlan}
                      disabled={generate.isPending}
                    >
                      {generate.isPending
                        ? "正在加入队列…"
                        : item.kind === "feedback"
                          ? "生成增量计划"
                          : "生成计划"}
                    </button>
                  </div>
                </>
              )}
            </div>
            <div
              className="intake-detail-scroll app-scroll-region"
              style={{
                display: "flex",
                flex: "1 1 auto",
                flexDirection: "column",
                minHeight: 0,
                marginTop: 20,
                overflowY: "auto",
                overscrollBehaviorY: "contain",
                paddingRight: 10,
                scrollbarGutter: "stable",
              }}
            >
              {item.kind === "feedback" && (
                <section className="feedback-context">
                  <span>关联需求</span>
                  {feedbackParent ? (
                    <button
                      type="button"
                      onClick={() => selectIntake(feedbackParent.id)}
                    >
                      {feedbackParent.title}
                    </button>
                  ) : (
                    <strong>关联需求已不存在</strong>
                  )}
                </section>
              )}
              {item.kind === "requirement" && relatedFeedback.length > 0 && (
                <section className="feedback-context related-feedback">
                  <span>关联反馈（{relatedFeedback.length}）</span>
                  <div>
                    {relatedFeedback.map((feedback) => (
                      <button
                        type="button"
                        key={feedback.id}
                        onClick={() => selectIntake(feedback.id)}
                      >
                        {feedback.title}
                        <Status value={feedback.status} />
                      </button>
                    ))}
                  </div>
                </section>
              )}
              <div className="intake-body">{item.body}</div>
            </div>
          </article>
        ) : (
          <Empty title="请选择一条需求" body="从左侧选择项目，查看详细内容。" />
        )}
      </section>
    </div>
  );
}
