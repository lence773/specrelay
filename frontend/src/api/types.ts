import type { CliProbeResponse, CliProvider } from './generated/types.gen'

export type {
  AsyncResponse,
  CliProbeResponse as CLIProbeResponse,
  CliProbeResult as CLIProbeResult,
  CliProvider as CLIProvider,
  DirectoryEntry,
  DirectoryListing,
  Error as APIError,
  Event as EventRecord,
  Intake,
  IntakeInput,
  Plan,
  PlanTask,
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
