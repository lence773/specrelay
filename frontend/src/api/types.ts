import type { CliProbeResponse, CliProvider } from './generated/types.gen'

export type {
  AsyncResponse,
  AgentRunObservabilityAggregates,
  AgentRunObservabilityExport,
  AgentRunObservabilityResponse,
  AgentRunObservabilitySummary,
  AgentRunProvider,
  CliProbeResponse as CLIProbeResponse,
  CliProbeResult as CLIProbeResult,
  CliProvider as CLIProvider,
  DirectoryEntry,
  DirectoryListing,
  Error as APIError,
  Event as EventRecord,
  FeedbackAssociationSummary,
  FeedbackCheckpointSummary,
  FeedbackContext,
  FeedbackCreateInput,
  FeedbackCreateResponse,
  FeedbackDiffSummary,
  FeedbackFileSummary,
  FeedbackIntakeSummary,
  FeedbackPlanSummary,
  FeedbackReference,
  FeedbackRevision,
  FeedbackRevisionDiscussionResult,
  FeedbackRevisionState,
  FeedbackRevisionStatus,
  FeedbackRevisionSummary,
  FeedbackTaskSummary,
  GetCheckpointResponse,
  GetPlanResponse,
  GetTaskResponse,
  Intake,
  IntakeInput,
  Plan,
  PlanExecutionContext,
  ExecutionContextDifference,
  ExecutionContextDriftReport,
  AcceptPlanExecutionContextResponse,
  PlanExecutionSnapshot,
  PlanExecutionSnapshotFile,
  PlanExecutionSnapshotHunk,
  PlanTask,
  McpAuthentication,
  McpConnectionInfo,
  McpDiagnostic,
  McpTokenStatus,
  McpTool,
  ObservabilityAgentRun,
  ObservabilityCostSummary,
  ObservabilityCurrencyCost,
  ObservabilityDurationSummary,
  ObservabilityDurationTrend,
  ObservabilityExportOptions,
  ObservabilityFailureCount,
  ObservabilityFilter,
  ObservabilityPagination,
  ObservabilityPlan,
  ObservabilityRate,
  ObservabilityRequirement,
  ObservabilityTask,
  ObservabilityTokenSummary,
  ObservabilityUsage,
  ObservabilityUsageGroup,
  ObservabilityUsageSummary,
  Project,
  ProjectSettings,
  RequirementDiscussionMessage,
  RequirementDiscussionRequest,
  RequirementDiscussionResult,
} from './generated/types.gen'

export type UUID = string

/**
 * The generated two-provider probe response plus the aggregate text consumed by
 * the existing API facade. The aggregate is diagnostic only.
 */
export type CLIProbeSummary = CliProbeResponse & {
  output:string
  exitCode:number
}

export interface AgentRun {
  id:UUID
  projectId:UUID
  jobId?:UUID
  taskId?:UUID
  provider:CliProvider|'validation'
  commandSummary:string
  pid?:number
  sessionId?:string
  status:'starting'|'running'|'succeeded'|'failed'|'cancelled'|'timed_out'
  exitCode?:number
  durationMs:number
  terminationReason?:string
  startedAt:string
  finishedAt?:string
  createdAt:string
  updatedAt:string
  version:number
}

export interface AgentRunLog {
  runId:UUID
  status:AgentRun['status']
  provider:AgentRun['provider']
  /** Complete log records, ordered from older to newer within this page. */
  lines:string[]
  sizeBytes:number
  hasMore:boolean
  /** Exclusive byte cursor used to retrieve the preceding page. */
  nextBefore?:number
  updatedAt:string
}

export type ObservabilityQuery = import('./generated/types.gen').QueryProjectObservabilityData['query']
export type ObservabilityExportRequest = NonNullable<import('./generated/types.gen').ExportProjectObservabilityData['query']>

export interface LocalObservabilityExport {
  blob:Blob
  filename:string
  contentType:string
}

export type FeedbackAssociationInput = Pick<
  import('./generated/types.gen').FeedbackCreateInput,
  'requirementId'|'planId'|'taskId'|'checkpointId'|'fileId'|'diffHunkId'|'diffLineSide'|'diffLineStart'|'diffLineEnd'
>

export type FeedbackDiffLineRange = {
  diffHunkId:UUID
  diffLineSide:NonNullable<import('./generated/types.gen').FeedbackCreateInput['diffLineSide']>
  diffLineStart:number
  diffLineEnd:number
}

export type FeedbackAssociationTarget =
  | {kind:'plan';id:UUID}
  | {kind:'task';id:UUID}
  | {kind:'checkpoint';id:UUID}

/**
 * Feedback discussions deliberately exclude caller-supplied CLI session IDs.
 * The server resolves a session only after matching project, feedback, and provider.
 */
export type FeedbackDiscussionRequest = Pick<
  import('./generated/types.gen').RequirementDiscussionRequest,
  'title'|'body'|'messages'|'provider'
>

export type PlanDetail = import('./generated/types.gen').GetPlanResponse
export type TaskDetail = import('./generated/types.gen').GetTaskResponse
export type CheckpointDetail = import('./generated/types.gen').GetCheckpointResponse
