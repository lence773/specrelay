import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type {
  CLIProvider,
  FeedbackAssociationInput,
  FeedbackContext,
  FeedbackCreateInput,
  FeedbackReference,
  Intake,
  PlanExecutionSnapshotHunk,
  Project,
  RequirementDiscussionMessage,
} from "../../api/types";
import {
  FileDiff,
  GitBranch,
  LinkIcon,
  MessageSquare,
  Warning,
} from "../../components/Icons";
import { Modal } from "../../components/Modal";
import { Status } from "../../components/Status";

export type FeedbackNavigationTarget =
  | { kind: "intake"; id: string }
  | { kind: "feedback"; id: string }
  | { kind: "plan"; id: string }
  | { kind: "task"; id: string; planId: string }
  | {
      kind: "checkpoint";
      id: string;
      requirementId: string;
      planId?: string;
      taskId?: string;
    };

export type FeedbackDraft = FeedbackAssociationInput & {
  requirementTitle?: string;
  planTitle?: string;
  taskTitle?: string;
  taskKey?: string;
  checkpointLabel?: string;
  filePath?: string;
  diffHeader?: string;
};

type ProviderChoice = CLIProvider | undefined;

type DiffLine = {
  key: string;
  content: string;
  prefix: string;
  oldLine?: number;
  newLine?: number;
};

type DiffSelection = {
  hunkId: string;
  side: "old" | "new";
  start: number;
  end: number;
};

const revisionLabels: Record<string, string> = {
  not_started: "未发起修订",
  requested: "已请求修订",
  planning: "修订规划中",
  ready: "修订计划就绪",
  running: "修订执行中",
  completed: "修订已完成",
  failed: "修订失败",
  blocked: "修订已阻塞",
  cancelled: "修订已取消",
  unknown: "修订状态未知",
};

function revisionLabel(value: string) {
  return revisionLabels[value] ?? value.replaceAll("_", " ");
}

function relationError(error: unknown) {
  const status = error && typeof error === "object" && "status" in error
    ? Number((error as { status?: unknown }).status)
    : 0;
  if (status === 403) return "关联对象无访问权限";
  if (status === 404) return "关联对象已不存在";
  return "关联链路无法读取，可能已失效或无权限";
}

function AssociationReview({ draft }: { draft: FeedbackDraft }) {
  const nodes = [
    {
      key: "requirement",
      label: "父需求",
      value: draft.requirementTitle ?? draft.requirementId,
      active: !!draft.requirementId,
    },
    {
      key: "plan",
      label: "计划",
      value: draft.planTitle ?? draft.planId,
      active: !!draft.planId,
    },
    {
      key: "task",
      label: "任务",
      value: draft.taskTitle
        ? `${draft.taskKey ? `${draft.taskKey} · ` : ""}${draft.taskTitle}`
        : draft.taskId,
      active: !!draft.taskId,
    },
    {
      key: "checkpoint",
      label: "检查点",
      value: draft.checkpointLabel ?? draft.checkpointId,
      active: !!draft.checkpointId,
    },
    {
      key: "file",
      label: "文件",
      value: draft.filePath ?? draft.fileId,
      active: !!draft.fileId,
    },
    {
      key: "diff",
      label: "Diff",
      value: draft.diffHeader ?? draft.diffHunkId,
      active: !!draft.diffHunkId,
    },
  ].filter((node) => node.active);
  return (
    <section className="feedback-association-review" aria-label="反馈关联对象">
      <div className="feedback-section-heading">
        <LinkIcon />
        <div>
          <strong>提交前检查关联对象</strong>
          <span>反馈会永久记录以下追溯链路。</span>
        </div>
      </div>
      <ol>
        {nodes.map((node) => (
          <li key={node.key}>
            <span>{node.label}</span>
            <strong title={node.value ?? undefined}>{node.value}</strong>
          </li>
        ))}
      </ol>
      {draft.diffLineSide &&
        draft.diffLineStart !== undefined &&
        draft.diffLineEnd !== undefined && (
          <div className="feedback-line-range">
            <FileDiff /> 精确范围：
            {draft.diffLineSide === "new" ? "新文件" : "原文件"}第{" "}
            {draft.diffLineStart}
            {draft.diffLineEnd === draft.diffLineStart
              ? ""
              : `–${draft.diffLineEnd}`}{" "}
            行
          </div>
        )}
    </section>
  );
}

export function FeedbackComposer({
  project,
  draft,
  onClose,
  onCreated,
}: {
  project: Project;
  draft: FeedbackDraft;
  onClose: () => void;
  onCreated?: (id: string) => void;
}) {
  const queryClient = useQueryClient();
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const create = useMutation({
    mutationFn: () => {
      const input: FeedbackCreateInput = {
        requirementId: draft.requirementId,
        title: title.trim(),
        body: body.trim(),
        ...(draft.planId ? { planId: draft.planId } : {}),
        ...(draft.taskId ? { taskId: draft.taskId } : {}),
        ...(draft.checkpointId ? { checkpointId: draft.checkpointId } : {}),
        ...(draft.fileId ? { fileId: draft.fileId } : {}),
        ...(draft.diffHunkId ? { diffHunkId: draft.diffHunkId } : {}),
        ...(draft.diffLineSide ? { diffLineSide: draft.diffLineSide } : {}),
        ...(draft.diffLineStart !== undefined
          ? { diffLineStart: draft.diffLineStart }
          : {}),
        ...(draft.diffLineEnd !== undefined
          ? { diffLineEnd: draft.diffLineEnd }
          : {}),
      };
      return api.createFeedback(project.id, input);
    },
    onSuccess: async (result) => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["intakes", project.id] }),
        queryClient.invalidateQueries({ queryKey: ["plans", project.id] }),
        draft.planId
          ? queryClient.invalidateQueries({ queryKey: ["plan", draft.planId] })
          : Promise.resolve(),
        draft.taskId
          ? queryClient.invalidateQueries({ queryKey: ["task", draft.taskId] })
          : Promise.resolve(),
        draft.checkpointId
          ? queryClient.invalidateQueries({
              queryKey: ["checkpoint", draft.checkpointId],
            })
          : Promise.resolve(),
      ]);
      onCreated?.(result.feedback.id);
      onClose();
    },
  });
  const valid = !!draft.requirementId && !!title.trim() && !!body.trim();
  return (
    <Modal title="创建关联反馈" onClose={onClose} wide>
      <form
        className="feedback-composer"
        onSubmit={(event) => {
          event.preventDefault();
          if (valid && !create.isPending) create.mutate();
        }}
      >
        <AssociationReview draft={draft} />
        <label>
          <span>反馈标题</span>
          <input
            autoFocus
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            placeholder="简要说明需要调整什么"
            maxLength={256}
          />
        </label>
        <label>
          <span>反馈说明</span>
          <textarea
            value={body}
            onChange={(event) => setBody(event.target.value)}
            placeholder="描述问题、期望结果和验收方式…"
            rows={7}
          />
        </label>
        <div className="feedback-safety-note">
          <GitBranch />
          <span>
            <strong>这不会修改原计划。</strong>
            提交后可在反馈详情中讨论并确认一个独立的增量修订。
          </span>
        </div>
        {create.error && (
          <div className="form-error" role="alert">
            创建反馈失败：{create.error.message}
          </div>
        )}
        <footer className="modal-actions">
          <button
            type="button"
            className="button ghost"
            onClick={onClose}
            disabled={create.isPending}
          >
            取消
          </button>
          <button
            className="button primary"
            disabled={!valid || create.isPending}
          >
            {create.isPending ? "正在创建…" : "创建反馈"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}

export function FeedbackReferenceList({
  items,
  onOpen,
  emptyText = "暂无关联反馈",
}: {
  items: FeedbackReference[];
  onOpen?: (id: string) => void;
  emptyText?: string;
}) {
  if (items.length === 0)
    return <div className="feedback-reference-empty">{emptyText}</div>;
  return (
    <div className="feedback-reference-list">
      {items.map((item) => (
        <button
          type="button"
          key={item.id}
          onClick={() => onOpen?.(item.id)}
          disabled={!onOpen}
        >
          <span>
            <MessageSquare />
            <strong>{item.title}</strong>
          </span>
          <span>
            <Status value={item.feedbackStatus} />
            <em className={`revision-state revision-${item.revisionStatus}`}>
              {revisionLabel(item.revisionStatus)}
            </em>
          </span>
        </button>
      ))}
    </div>
  );
}

function TraceNode({
  label,
  title,
  meta,
  missing = false,
  missingText = "已失效或无权限",
  onClick,
}: {
  label: string;
  title?: string;
  meta?: React.ReactNode;
  missing?: boolean;
  missingText?: string;
  onClick?: () => void;
}) {
  return (
    <li className={missing ? "trace-node invalid" : "trace-node"}>
      <span>{label}</span>
      <button
        type="button"
        onClick={onClick}
        disabled={missing || !onClick}
        aria-disabled={missing || !onClick}
      >
        <strong>{missing ? missingText : title}</strong>
        {meta && <small>{meta}</small>}
      </button>
    </li>
  );
}

function parsePatch(hunk: PlanExecutionSnapshotHunk): {
  lines: DiffLine[];
  truncated: boolean;
} {
  const patchCharacterLimit = 120_000;
  const boundedPatch = hunk.patch.slice(0, patchCharacterLimit);
  const raw = boundedPatch.split("\n");
  const patchLines = raw[0]?.startsWith("@@") ? raw.slice(1) : raw;
  let oldLine = hunk.oldStartLine;
  let newLine = hunk.newStartLine;
  const limited = patchLines.slice(0, 240);
  const lines: DiffLine[] = [];
  limited.forEach((value, index) => {
    if (value.startsWith("\\ No newline")) {
      lines.push({ key: `note-${index}`, prefix: "\\", content: value });
      return;
    }
    const prefix = value[0] ?? " ";
    const content = (
      prefix === "+" || prefix === "-" || prefix === " "
        ? value.slice(1)
        : value
    ).slice(0, 500);
    if (prefix === "+") {
      lines.push({ key: `new-${newLine}-${index}`, prefix, content, newLine });
      newLine += 1;
      return;
    }
    if (prefix === "-") {
      lines.push({ key: `old-${oldLine}-${index}`, prefix, content, oldLine });
      oldLine += 1;
      return;
    }
    lines.push({
      key: `ctx-${oldLine}-${newLine}-${index}`,
      prefix: " ",
      content,
      oldLine,
      newLine,
    });
    oldLine += 1;
    newLine += 1;
  });
  return {
    lines,
    truncated:
      hunk.patch.length > patchCharacterLimit ||
      patchLines.length > limited.length ||
      patchLines.some((line) => line.length > 501),
  };
}

function boundSnippet(value: string) {
  const sourceLines = value.slice(0, 120_000).split("\n");
  const limitedLines = sourceLines.slice(0, 240);
  const text = limitedLines.map((line) => line.slice(0, 500)).join("\n");
  return {
    text,
    truncated:
      value.length > 120_000 ||
      sourceLines.length > limitedLines.length ||
      sourceLines.some((line) => line.length > 500),
  };
}

function selectionContains(
  selection: DiffSelection | undefined,
  hunkId: string,
  side: "old" | "new",
  line: number,
) {
  return (
    !!selection &&
    selection.hunkId === hunkId &&
    selection.side === side &&
    line >= selection.start &&
    line <= selection.end
  );
}

export function CheckpointDiffView({
  project,
  checkpointId,
  association,
  onClose,
  onOpenFeedback,
  onCreated,
  initialFileId,
  initialHunkId,
}: {
  project: Project;
  checkpointId: string;
  association?: Partial<FeedbackDraft>;
  onClose?: () => void;
  onOpenFeedback?: (id: string) => void;
  onCreated?: (id: string) => void;
  initialFileId?: string;
  initialHunkId?: string;
}) {
  const detail = useQuery({
    queryKey: ["checkpoint", checkpointId],
    queryFn: () => api.checkpointDiff(checkpointId),
    retry: false,
  });
  const [selectedFileId, setSelectedFileId] = useState(initialFileId);
  const [selectedHunkId, setSelectedHunkId] = useState(initialHunkId);
  const [selection, setSelection] = useState<DiffSelection>();
  const [composerDraft, setComposerDraft] = useState<FeedbackDraft>();
  useEffect(() => {
    setSelectedFileId(initialFileId);
    setSelectedHunkId(initialHunkId);
    setSelection(undefined);
    setComposerDraft(undefined);
  }, [checkpointId, initialFileId, initialHunkId]);
  useEffect(() => {
    if (detail.data && !selectedFileId)
      setSelectedFileId(detail.data.checkpoint.files[0]?.id);
  }, [detail.data, selectedFileId]);
  const checkpoint = detail.data?.checkpoint;
  const checkpointFeedback = detail.data?.feedback ?? [];
  const selectedFile =
    checkpoint?.files.find((file) => file.id === selectedFileId) ??
    checkpoint?.files[0];
  const selectedHunk =
    selectedFile?.hunks.find((hunk) => hunk.id === selectedHunkId) ??
    selectedFile?.hunks[0];
  useEffect(() => {
    if (selectedHunk && selectedHunk.id !== selectedHunkId)
      setSelectedHunkId(selectedHunk.id);
  }, [selectedHunk, selectedHunkId]);
  const parsed = useMemo(
    () => (selectedHunk ? parsePatch(selectedHunk) : undefined),
    [selectedHunk],
  );
  const requirementId =
    association?.requirementId ??
    (checkpoint as unknown as { requirementId?: string } | undefined)
      ?.requirementId ??
    checkpoint?.intakeId ??
    "";
  const baseDraft: FeedbackDraft = {
    requirementId,
    requirementTitle: association?.requirementTitle,
    planId: association?.planId ?? checkpoint?.planId,
    planTitle: association?.planTitle,
    taskId: association?.taskId ?? checkpoint?.taskId,
    taskTitle: association?.taskTitle,
    taskKey: association?.taskKey,
    checkpointId,
    checkpointLabel:
      association?.checkpointLabel ??
      (checkpoint ? `检查点 #${checkpoint.sequence}` : undefined),
  };
  const chooseLine = (side: "old" | "new", line: number) => {
    if (!selectedHunk) return;
    setSelection((current) => {
      if (
        !current ||
        current.hunkId !== selectedHunk.id ||
        current.side !== side
      )
        return { hunkId: selectedHunk.id, side, start: line, end: line };
      return {
        hunkId: selectedHunk.id,
        side,
        start: Math.min(current.start, line),
        end: Math.max(current.end, line),
      };
    });
  };
  const openComposer = (precise: boolean) => {
    if (!selectedFile || !selectedHunk) return;
    setComposerDraft({
      ...baseDraft,
      fileId: selectedFile.id,
      filePath: selectedFile.path,
      diffHunkId: selectedHunk.id,
      diffHeader: selectedHunk.header,
      ...(precise && selection
        ? {
            diffLineSide: selection.side,
            diffLineStart: selection.start,
            diffLineEnd: selection.end,
          }
        : {}),
    });
  };
  const openFileComposer = () => {
    if (!selectedFile) return;
    setComposerDraft({
      ...baseDraft,
      fileId: selectedFile.id,
      filePath: selectedFile.path,
    });
  };

  const content = detail.isLoading ? (
    <div className="loading">正在加载受限 Diff…</div>
  ) : detail.error ? (
    <div className="broken-relation" role="alert">
      <Warning />
      <div>
        <strong>{relationError(detail.error)}</strong>
        <p>无法打开检查点 {checkpointId}。损坏链接和修订操作已禁用。</p>
      </div>
    </div>
  ) : checkpoint ? (
    <div className="checkpoint-diff-view">
      <header className="diff-view-header">
        <div>
          <span className="eyebrow">不可变执行检查点</span>
          <h2>变更与 Diff · #{checkpoint.sequence}</h2>
          <p>
            {checkpoint.additions ?? 0} 行新增，{checkpoint.deletions ?? 0}{" "}
            行删除；仅展示受长度限制的补丁，不加载完整文件或 Agent 日志。
          </p>
        </div>
        {onClose && (
          <button
            type="button"
            className="button ghost small"
            onClick={onClose}
          >
            返回反馈
          </button>
        )}
      </header>
      <section
        className="checkpoint-feedback-summary"
        aria-label="检查点关联反馈"
      >
        <div>
          <strong>关联反馈（{checkpointFeedback.length}）</strong>
          <span>可反向查看反馈和后续修订状态</span>
        </div>
        <FeedbackReferenceList
          items={checkpointFeedback}
          onOpen={onOpenFeedback}
        />
      </section>
      {checkpoint.files.length === 0 ? (
        <div className="diff-empty">此检查点没有可展示的文件变更。</div>
      ) : (
        <div className="diff-browser">
          <aside className="diff-files" aria-label="检查点文件">
            {checkpoint.files.map((file) => (
              <button
                type="button"
                key={file.id}
                className={file.id === selectedFile?.id ? "selected" : ""}
                onClick={() => {
                  setSelectedFileId(file.id);
                  setSelectedHunkId(file.hunks[0]?.id);
                  setSelection(undefined);
                }}
              >
                <span>
                  <strong>{file.path}</strong>
                  <small>
                    {file.binary
                      ? "二进制文件"
                      : `${file.hunks.length} 个 Diff 区块`}
                  </small>
                </span>
                <em>
                  <b>+{file.additions}</b>
                  <i>-{file.deletions}</i>
                </em>
              </button>
            ))}
          </aside>
          <section className="diff-content" aria-label="受限 Diff">
            {selectedFile?.binary ? (
              <div className="diff-empty">
                <strong>二进制文件不展示内容</strong>
                <span>仍可针对文件级别创建反馈，但不会复制大文件内容。</span>
                <button
                  type="button"
                  className="button secondary small"
                  onClick={openFileComposer}
                  disabled={!requirementId}
                >
                  为文件创建反馈
                </button>
              </div>
            ) : selectedFile && selectedFile.hunks.length === 0 ? (
              <div className="diff-empty">
                <strong>此文件没有文本 Diff 区块</strong>
                <span>可创建文件级反馈，不会加载完整文件内容。</span>
                <button
                  type="button"
                  className="button secondary small"
                  onClick={openFileComposer}
                  disabled={!requirementId}
                >
                  为文件创建反馈
                </button>
              </div>
            ) : (
              <>
                <div className="diff-hunk-tabs">
                  {selectedFile?.hunks.map((hunk) => (
                    <button
                      type="button"
                      key={hunk.id}
                      className={hunk.id === selectedHunk?.id ? "active" : ""}
                      onClick={() => {
                        setSelectedHunkId(hunk.id);
                        setSelection(undefined);
                      }}
                    >
                      {hunk.header}
                    </button>
                  ))}
                </div>
                {selectedHunk && parsed && (
                  <div
                    className="diff-table"
                    role="table"
                    aria-label={`${selectedFile?.path} Diff`}
                  >
                    <div className="diff-table-header">
                      <code>{selectedHunk.header}</code>
                      <span>点击旧/新行号选择连续范围</span>
                    </div>
                    {parsed.lines.map((line) => (
                      <div
                        role="row"
                        className={`diff-row diff-${line.prefix === "+" ? "add" : line.prefix === "-" ? "remove" : "context"}`}
                        key={line.key}
                      >
                        <button
                          type="button"
                          className={
                            line.oldLine !== undefined &&
                            selectionContains(
                              selection,
                              selectedHunk.id,
                              "old",
                              line.oldLine,
                            )
                              ? "selected"
                              : ""
                          }
                          disabled={line.oldLine === undefined}
                          aria-label={
                            line.oldLine === undefined
                              ? undefined
                              : `选择旧行 ${line.oldLine}`
                          }
                          onClick={() =>
                            line.oldLine !== undefined &&
                            chooseLine("old", line.oldLine)
                          }
                        >
                          {line.oldLine ?? ""}
                        </button>
                        <button
                          type="button"
                          className={
                            line.newLine !== undefined &&
                            selectionContains(
                              selection,
                              selectedHunk.id,
                              "new",
                              line.newLine,
                            )
                              ? "selected"
                              : ""
                          }
                          disabled={line.newLine === undefined}
                          aria-label={
                            line.newLine === undefined
                              ? undefined
                              : `选择新行 ${line.newLine}`
                          }
                          onClick={() =>
                            line.newLine !== undefined &&
                            chooseLine("new", line.newLine)
                          }
                        >
                          {line.newLine ?? ""}
                        </button>
                        <code>
                          <span>{line.prefix}</span>
                          {line.content}
                        </code>
                      </div>
                    ))}
                    {parsed.truncated && (
                      <div className="diff-limit-notice">
                        <Warning /> Diff
                        已按安全上限截断；不会展示或复制完整大文件内容。
                      </div>
                    )}
                  </div>
                )}
                <div className="diff-feedback-actions">
                  <div>
                    {selection ? (
                      <strong>
                        已选择{selection.side === "new" ? "新" : "旧"}文件第{" "}
                        {selection.start}
                        {selection.end === selection.start
                          ? ""
                          : `–${selection.end}`}{" "}
                        行
                      </strong>
                    ) : (
                      <span>可创建区块级反馈，或先点击行号选择精确范围。</span>
                    )}
                  </div>
                  <button
                    type="button"
                    className="button secondary"
                    onClick={() => openComposer(false)}
                    disabled={!selectedHunk || !requirementId}
                  >
                    创建区块反馈
                  </button>
                  <button
                    type="button"
                    className="button primary"
                    onClick={() => openComposer(true)}
                    disabled={!selection || !requirementId}
                  >
                    为所选行创建精确反馈
                  </button>
                </div>
              </>
            )}
          </section>
        </div>
      )}
    </div>
  ) : null;

  return (
    <>
      {content}
      {composerDraft && (
        <FeedbackComposer
          project={project}
          draft={composerDraft}
          onClose={() => setComposerDraft(undefined)}
          onCreated={(id) => {
            onCreated?.(id);
            onOpenFeedback?.(id);
          }}
        />
      )}
    </>
  );
}

function revisionIntakeById(intakes: Intake[] | undefined, id: string) {
  return intakes?.find((item) => item.id === id);
}

export function FeedbackDetailPanel({
  project,
  feedbackId,
  intakes,
  onNavigate,
}: {
  project: Project;
  feedbackId: string;
  intakes?: Intake[];
  onNavigate?: (target: FeedbackNavigationTarget) => void;
}) {
  const queryClient = useQueryClient();
  const context = useQuery({
    queryKey: ["feedback", project.id, feedbackId],
    queryFn: () => api.feedback(project.id, feedbackId),
    retry: false,
    enabled: typeof api.feedback === "function",
  });
  const [messages, setMessages] = useState<RequirementDiscussionMessage[]>([]);
  const [input, setInput] = useState("");
  const [provider, setProvider] = useState<ProviderChoice>();
  const [ready, setReady] = useState(false);
  const [draftTitle, setDraftTitle] = useState("");
  const [draftBody, setDraftBody] = useState("");
  const [openCheckpoint, setOpenCheckpoint] = useState(false);
  const [createdRevision, setCreatedRevision] =
    useState<
      NonNullable<
        Awaited<ReturnType<typeof api.createFeedbackRevision>>["revision"]
      >
    >();
  const [queuedRevisionIntakeIds, setQueuedRevisionIntakeIds] = useState<
    Set<string>
  >(new Set());
  useEffect(() => {
    setMessages([]);
    setInput("");
    setProvider(undefined);
    setReady(false);
    setDraftTitle("");
    setDraftBody("");
    setOpenCheckpoint(false);
    setCreatedRevision(undefined);
    setQueuedRevisionIntakeIds(new Set());
  }, [feedbackId]);
  useEffect(() => {
    if (context.data?.feedback.id === feedbackId) {
      setDraftTitle(`修订：${context.data.feedback.title}`);
      setDraftBody(context.data.feedback.body);
    }
  }, [context.data, feedbackId]);
  const discuss = useMutation({
    mutationFn: (next: RequirementDiscussionMessage[]) =>
      api.discussFeedback(project.id, feedbackId, {
        title: draftTitle,
        body: draftBody,
        messages: next,
        ...(provider ? { provider } : {}),
      }),
    onSuccess: (result, next) => {
      setMessages([...next, { role: "assistant", content: result.reply }]);
      setReady(result.ready);
      setDraftTitle(result.title);
      setDraftBody(result.body);
    },
  });
  const confirm = useMutation({
    mutationFn: (next: RequirementDiscussionMessage[]) =>
      api.createFeedbackRevision(project.id, feedbackId, {
        title: draftTitle,
        body: draftBody,
        messages: next,
        ...(provider ? { provider } : {}),
      }),
    onSuccess: async (result, next) => {
      setMessages([...next, { role: "assistant", content: result.reply }]);
      setReady(result.ready);
      setDraftTitle(result.title);
      setDraftBody(result.body);
      if (result.revision) {
        setCreatedRevision(result.revision);
        await Promise.all([
          queryClient.invalidateQueries({
            queryKey: ["feedback", project.id, feedbackId],
          }),
          queryClient.invalidateQueries({ queryKey: ["intakes", project.id] }),
          queryClient.invalidateQueries({ queryKey: ["plans", project.id] }),
        ]);
      }
    },
  });
  const generate = useMutation({
    mutationFn: (intake: Intake) =>
      api.generateFeedbackRevisionPlan(intake, provider),
    onSuccess: async (_, intake) => {
      setQueuedRevisionIntakeIds((current) => {
        const next = new Set(current);
        next.add(intake.id);
        return next;
      });
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ["feedback", project.id, feedbackId],
        }),
        queryClient.invalidateQueries({ queryKey: ["intakes", project.id] }),
        queryClient.invalidateQueries({ queryKey: ["plans", project.id] }),
      ]);
    },
  });
  const send = () => {
    const text = input.trim();
    if (!text || discuss.isPending) return;
    const next = [...messages, { role: "user" as const, content: text }];
    setMessages(next);
    setInput("");
    setReady(false);
    discuss.mutate(next);
  };

  if (context.isLoading)
    return <div className="loading">正在加载反馈追溯链路…</div>;
  if (context.error)
    return (
      <div className="broken-relation" role="alert">
        <Warning />
        <div>
          <strong>{relationError(context.error)}</strong>
          <p>
            反馈详情无法验证完整关联链路。已禁用损坏链接、讨论确认和修订操作。
          </p>
        </div>
      </div>
    );
  if (!context.data) return null;
  const data: FeedbackContext = context.data;
  const association = data.association;
  const missingRevisionIntakeIds = new Set(
    intakes
      ? data.revision.items
          .filter(
            (item) => !intakes.some((intake) => intake.id === item.requirementId),
          )
          .map((item) => item.requirementId)
      : [],
  );
  const broken =
    !!(association.planId && !data.plan) ||
    !!(association.taskId && !data.task) ||
    !!(association.checkpointId && !data.checkpoint) ||
    !!(association.fileId && !data.file) ||
    !!(association.diffHunkId && !data.diff) ||
    missingRevisionIntakeIds.size > 0;
  const checkpointAssociation: Partial<FeedbackDraft> = {
    requirementId: association.requirementId,
    requirementTitle: data.requirement.title,
    planId: association.planId,
    planTitle: data.plan?.title,
    taskId: association.taskId,
    taskTitle: data.task?.title,
    taskKey: data.task?.taskKey,
    checkpointLabel: data.checkpoint
      ? `检查点 #${data.checkpoint.sequence}`
      : undefined,
  };
  const latestCreated = createdRevision?.intake;
  const latestPlanQueued = !!(
    createdRevision?.job ||
    (latestCreated && queuedRevisionIntakeIds.has(latestCreated.id))
  );
  const remainingRevisionItems = data.revision.items.filter(
    (item) => item.requirementId !== latestCreated?.id,
  );
  const boundedDiffSnippet = data.diff?.snippet
    ? boundSnippet(data.diff.snippet)
    : undefined;

  if (openCheckpoint && association.checkpointId)
    return (
      <CheckpointDiffView
        project={project}
        checkpointId={association.checkpointId}
        association={checkpointAssociation}
        initialFileId={association.fileId}
        initialHunkId={association.diffHunkId}
        onClose={() => setOpenCheckpoint(false)}
        onOpenFeedback={(id) => onNavigate?.({ kind: "feedback", id })}
      />
    );

  return (
    <div className="feedback-detail-panel">
      {broken && (
        <div className="broken-relation compact" role="alert">
          <Warning />
          <div>
            <strong>部分关联对象已失效或无权限</strong>
            <p>损坏节点不可跳转；在链路恢复前不能创建修订。</p>
          </div>
        </div>
      )}
      <header>
        <div>
          <span className="eyebrow">反馈详情与完整追溯</span>
          <h2>{data.feedback.title}</h2>
          <div className="detail-meta">
            <Status value={data.feedback.status} />
            <span>
              {new Date(data.feedback.createdAt).toLocaleString("zh-CN")}
            </span>
          </div>
        </div>
      </header>
      <section className="feedback-trace" aria-label="完整追溯链路">
        <ol>
          <TraceNode
            label="原需求"
            title={data.requirement.title}
            meta={<Status value={data.requirement.status} />}
            onClick={() =>
              onNavigate?.({ kind: "intake", id: data.requirement.id })
            }
          />
          {association.planId && (
            <TraceNode
              label="原计划"
              title={data.plan?.title}
              meta={data.plan && <Status value={data.plan.status} />}
              missing={!data.plan}
              onClick={() =>
                data.plan && onNavigate?.({ kind: "plan", id: data.plan.id })
              }
            />
          )}
          {association.taskId && (
            <TraceNode
              label="任务"
              title={
                data.task
                  ? `${data.task.taskKey} · ${data.task.title}`
                  : undefined
              }
              meta={data.task && <Status value={data.task.status} />}
              missing={!data.task}
              onClick={() =>
                data.task &&
                data.plan &&
                onNavigate?.({
                  kind: "task",
                  id: data.task.id,
                  planId: data.plan.id,
                })
              }
            />
          )}
          {association.checkpointId && (
            <TraceNode
              label="检查点"
              title={
                data.checkpoint
                  ? `检查点 #${data.checkpoint.sequence}`
                  : undefined
              }
              meta={data.checkpoint?.kind}
              missing={!data.checkpoint}
              onClick={() => data.checkpoint && setOpenCheckpoint(true)}
            />
          )}
          {association.fileId && (
            <TraceNode
              label="文件"
              title={data.file?.path}
              meta={
                data.file &&
                `${data.file.status} · +${data.file.additions} -${data.file.deletions}`
              }
              missing={!data.file}
              onClick={() => data.file && setOpenCheckpoint(true)}
            />
          )}
          {association.diffHunkId && (
            <TraceNode
              label="Diff 行"
              title={data.diff?.header}
              meta={
                data.diff?.side && data.diff.startLine !== undefined
                  ? `${data.diff.side === "new" ? "新" : "旧"}文件 ${data.diff.startLine}${data.diff.endLine === data.diff.startLine ? "" : `–${data.diff.endLine}`} 行`
                  : "区块级反馈"
              }
              missing={!data.diff}
              onClick={() => data.diff && setOpenCheckpoint(true)}
            />
          )}
          <TraceNode
            label="反馈"
            title={data.feedback.title}
            meta={<Status value={data.feedback.status} />}
          />
          {data.revision.items.map((item) => {
            const revisionIntake = revisionIntakeById(
              intakes,
              item.requirementId,
            );
            const missingRevisionIntake = missingRevisionIntakeIds.has(
              item.requirementId,
            );
            return (
              <span className="trace-revision-pair" key={item.id}>
                <TraceNode
                  label="修订 Intake"
                  title={item.requirementTitle}
                  meta={
                    <>
                      <Status value={item.intakeStatus} />
                      <em>{revisionLabel(item.currentStatus)}</em>
                    </>
                  }
                  missing={missingRevisionIntake}
                  onClick={() =>
                    !missingRevisionIntake &&
                    onNavigate?.({ kind: "intake", id: item.requirementId })
                  }
                />
                <TraceNode
                  label="修订 Plan"
                  title={
                    item.planId
                      ? `增量计划 · ${item.requirementTitle}`
                      : undefined
                  }
                  meta={item.planStatus && <Status value={item.planStatus} />}
                  missing={!item.planId}
                  missingText="尚未生成"
                  onClick={() =>
                    item.planId && onNavigate?.({ kind: "plan", id: item.planId })
                  }
                />
              </span>
            );
          })}
        </ol>
      </section>
      {boundedDiffSnippet && (
        <section className="feedback-bounded-snippet">
          <div>
            <FileDiff />
            <strong>已记录的受限 Diff 片段</strong>
          </div>
          <pre>{boundedDiffSnippet.text}</pre>
          <small>
            仅保存所选行或受限区块，不包含完整文件和 Agent 日志。
            {boundedDiffSnippet.truncated ? " 当前片段已按安全上限截断。" : ""}
          </small>
        </section>
      )}
      <section className="feedback-body-card">
        <span>反馈说明</span>
        <p>{data.feedback.body || "暂无说明"}</p>
      </section>
      <section className="revision-explainer">
        <GitBranch />
        <div>
          <strong>原计划不会被修改</strong>
          <p>
            讨论确认后会创建新的增量修订
            Intake。只有为它生成一份新计划，并由用户运行该新计划后，才会进入文件执行流程。
          </p>
        </div>
      </section>
      <section className="feedback-discussion" aria-label="反馈修订讨论">
        <header>
          <div>
            <strong>讨论增量修订</strong>
            <span>只读分析，不修改文件</span>
          </div>
          <div className="provider-inline">
            <button
              type="button"
              className={provider === undefined ? "active" : ""}
              onClick={() => setProvider(undefined)}
            >
              项目默认
            </button>
            <button
              type="button"
              className={provider === "codex" ? "active" : ""}
              onClick={() => setProvider("codex")}
            >
              Codex
            </button>
            <button
              type="button"
              className={provider === "claude" ? "active" : ""}
              onClick={() => setProvider("claude")}
            >
              Claude
            </button>
          </div>
        </header>
        {messages.length > 0 && (
          <div className="feedback-discussion-messages">
            {messages.map((message, index) => (
              <div key={index} className={message.role}>
                <strong>{message.role === "user" ? "你" : "CLI"}</strong>
                <p>{message.content}</p>
              </div>
            ))}
          </div>
        )}
        <label>
          <span>本轮消息</span>
          <textarea
            value={input}
            onChange={(event) => setInput(event.target.value)}
            placeholder="说明希望如何修正，或回答 CLI 的澄清问题…"
            rows={4}
            disabled={broken || discuss.isPending || confirm.isPending}
          />
        </label>
        {(discuss.error || confirm.error) && (
          <div className="form-error" role="alert">
            讨论失败：{(discuss.error ?? confirm.error)?.message}
          </div>
        )}
        <div className="feedback-discussion-actions">
          <button
            type="button"
            className="button secondary"
            onClick={send}
            disabled={
              broken || !input.trim() || discuss.isPending || confirm.isPending
            }
          >
            {discuss.isPending ? "分析中…" : "发送并讨论"}
          </button>
          <button
            type="button"
            className="button primary"
            onClick={() => {
              const next = [
                ...messages,
                {
                  role: "user" as const,
                  content: "确认以上增量修订内容，请创建独立修订。",
                },
              ];
              confirm.mutate(next);
            }}
            disabled={
              broken || !ready || confirm.isPending || !!createdRevision
            }
          >
            {confirm.isPending ? "正在创建修订…" : "确认并创建增量修订"}
          </button>
        </div>
        {ready && !createdRevision && (
          <div className="discussion-ready">
            <strong>增量修订已足够明确</strong>
            <span>请检查下方标题和说明，再确认创建。原计划保持不变。</span>
          </div>
        )}
        <div className="revision-draft">
          <label>
            <span>修订标题</span>
            <input
              value={draftTitle}
              onChange={(event) => setDraftTitle(event.target.value)}
              disabled={broken || confirm.isPending}
            />
          </label>
          <label>
            <span>独立修订说明</span>
            <textarea
              value={draftBody}
              onChange={(event) => setDraftBody(event.target.value)}
              rows={6}
              disabled={broken || confirm.isPending}
            />
          </label>
        </div>
      </section>
      {(latestCreated || remainingRevisionItems.length > 0) && (
        <section className="revision-result" aria-label="后续修订状态">
          <header>
            <div>
              <strong>后续修订</strong>
              <span>
                当前状态：{revisionLabel(data.revision.currentStatus)}
              </span>
            </div>
          </header>
          {latestCreated && (
            <article>
              <div>
                <strong>{latestCreated.title}</strong>
                <Status value={latestCreated.status} />
              </div>
              <p>
                {latestPlanQueued
                  ? "新的增量计划已加入生成队列；生成后仍需运行新计划才会执行文件变更。"
                  : "修订 Intake 已创建。下一步先生成独立的新计划。"}
              </p>
              {!latestPlanQueued && (
                <button
                  type="button"
                  className="button primary"
                  onClick={() => generate.mutate(latestCreated)}
                  disabled={generate.isPending}
                >
                  {generate.isPending ? "正在加入队列…" : "生成新的增量计划"}
                </button>
              )}
            </article>
          )}
          {generate.error && (
            <div className="form-error" role="alert">
              生成新的增量计划失败：{generate.error.message}
            </div>
          )}
          {remainingRevisionItems.map((item) => {
            const revisionIntake = revisionIntakeById(
              intakes,
              item.requirementId,
            );
            const planQueued = queuedRevisionIntakeIds.has(item.requirementId);
            return (
              <article key={item.id}>
                <div>
                  <button
                    type="button"
                    onClick={() =>
                      onNavigate?.({ kind: "intake", id: item.requirementId })
                    }
                  >
                    {item.requirementTitle}
                  </button>
                  <em
                    className={`revision-state revision-${item.currentStatus}`}
                  >
                    {revisionLabel(item.currentStatus)}
                  </em>
                </div>
                <div className="revision-result-actions">
                  {item.planId ? (
                    <button
                      type="button"
                      className="button small"
                      onClick={() =>
                        onNavigate?.({ kind: "plan", id: item.planId! })
                      }
                    >
                      查看新计划
                    </button>
                  ) : planQueued ? (
                    <span>新的增量计划已加入生成队列</span>
                  ) : revisionIntake &&
                    ["open", "plan_failed"].includes(revisionIntake.status) ? (
                    <button
                      type="button"
                      className="button small"
                      onClick={() => generate.mutate(revisionIntake)}
                      disabled={generate.isPending}
                    >
                      生成新的增量计划
                    </button>
                  ) : (
                    <span>等待修订 Intake 可用</span>
                  )}
                </div>
              </article>
            );
          })}
        </section>
      )}
    </div>
  );
}

function RequirementFeedbackRow({
  project,
  feedback,
  onOpen,
}: {
  project: Project;
  feedback: Intake;
  onOpen: (id: string) => void;
}) {
  const context = useQuery({
    queryKey: ["feedback", project.id, feedback.id],
    queryFn: () => api.feedback(project.id, feedback.id),
    retry: false,
    enabled: typeof api.feedback === "function",
  });
  return (
    <button
      type="button"
      onClick={() => onOpen(feedback.id)}
      disabled={context.isError}
    >
      <span>
        <strong>{feedback.title}</strong>
        <Status value={feedback.status} />
      </span>
      {context.isError ? (
        <em className="invalid-reference">关联已失效或无权限</em>
      ) : (
        <em
          className={`revision-state revision-${context.data?.revision.currentStatus ?? "not_started"}`}
        >
          {revisionLabel(context.data?.revision.currentStatus ?? "not_started")}
        </em>
      )}
    </button>
  );
}

export function RequirementFeedbackPanel({
  project,
  feedback,
  onOpen,
}: {
  project: Project;
  feedback: Intake[];
  onOpen: (id: string) => void;
}) {
  return (
    <section className="requirement-feedback-panel">
      <header>
        <div>
          <strong>关联反馈（{feedback.length}）</strong>
          <span>反馈状态与后续修订状态</span>
        </div>
      </header>
      {feedback.length === 0 ? (
        <div className="feedback-reference-empty">暂无关联反馈</div>
      ) : (
        <div>
          {feedback.map((item) => (
            <RequirementFeedbackRow
              key={item.id}
              project={project}
              feedback={item}
              onOpen={onOpen}
            />
          ))}
        </div>
      )}
    </section>
  );
}
