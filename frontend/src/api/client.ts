import { client } from './generated/client.gen'
import {
  createFeedback,
  createIntake,
  createProject,
  discussRequirement,
  exchangeAccessToken,
  diagnoseMcp,
  getMcpConnectionInfo,
  generatePlan,
  getCheckpoint,
  getFeedbackContext,
  getPlan,
  getPlanExecutionContext,
  acceptPlanExecutionContext,
  getProjectSettings,
  getTask,
  listDirectories,
  listEvents,
  listIntakes,
  listPlans,
  listProjects,
  probeAgent,
  queryProjectObservability,
  runPlan,
  runTask,
  rotateMcpToken,
  setProjectAutomation,
  updateIntake,
  updateProject,
  updateProjectSettings,
  uploadAttachment,
} from './generated/sdk.gen'
import type { EventPage } from './generated/types.gen'
import type {
  APIError,
  AgentRun,
  AgentRunLog,
  CLIProbeResponse,
  CLIProbeSummary,
  CLIProvider,
  DirectoryListing,
  AsyncResponse,
  CheckpointDetail,
  FeedbackAssociationTarget,
  FeedbackContext,
  FeedbackCreateInput,
  FeedbackCreateResponse,
  FeedbackDiscussionRequest,
  FeedbackReference,
  Intake,
  IntakeInput,
  Plan,
  PlanExecutionContext,
  PlanTask,
  AgentRunObservabilityResponse,
  ObservabilityExportRequest,
  ObservabilityQuery,
  LocalObservabilityExport,
  McpConnectionInfo,
  McpDiagnostic,
  PlanDetail,
  Project,
  ProjectSettings,
  RequirementDiscussionRequest,
  RequirementDiscussionResult,
  TaskDetail,
  UUID,
} from './types'

client.setConfig({baseUrl:'/api/v1',credentials:'same-origin'})

export class ClientError extends Error {
  constructor(public status:number,public body:APIError){super(body.message);this.name='ClientError'}
}

export function isExecutionContextDrift(error:unknown):error is ClientError {
  return error instanceof ClientError&&error.status===409&&error.body.code==='execution_context_drift'
}

type SDKResult<T>={data?:T;error?:unknown;response?:Response}

function isAPIError(value:unknown):value is APIError {
  if(!value||typeof value!=='object')return false
  const candidate=value as Partial<APIError>
  return typeof candidate.code==='string'&&typeof candidate.message==='string'&&typeof candidate.requestId==='string'
}

async function unwrap<T>(request:Promise<SDKResult<T>>):Promise<T>{
  const result=await request
  if(result.error!==undefined){
    const body=isAPIError(result.error)?result.error:{code:'http_error',message:result.error instanceof Error?result.error.message:String(result.error||result.response?.statusText||'Request failed'),requestId:result.response?.headers.get('X-Request-ID')??''}
    throw new ClientError(result.response?.status??0,body)
  }
  return result.data as T
}

async function getJSON<T>(path:string):Promise<T>{
  const response=await fetch(path,{credentials:'same-origin'})
  const body=await response.json().catch(()=>undefined)
  if(!response.ok){
    const error=isAPIError(body)?body:{code:'http_error',message:body?.message||response.statusText||'请求失败',requestId:response.headers.get('X-Request-ID')??''}
    throw new ClientError(response.status,error)
  }
  return body as T
}

async function downloadObservability(projectId:UUID,input:ObservabilityExportRequest):Promise<LocalObservabilityExport>{
  const query=new URLSearchParams()
  for(const [key,value] of Object.entries(input))if(value!==undefined)query.set(key,String(value))
  const response=await fetch(`/api/v1/projects/${encodeURIComponent(projectId)}/observability/export?${query}`,{credentials:'same-origin'})
  if(!response.ok){
    const body=await response.json().catch(()=>undefined)
    const error=isAPIError(body)?body:{code:'http_error',message:body?.message||response.statusText||'请求失败',requestId:response.headers.get('X-Request-ID')??''}
    throw new ClientError(response.status,error)
  }
  const disposition=response.headers.get('Content-Disposition')??''
  const filename=/filename="?([^";]+)"?/i.exec(disposition)?.[1]??`specrelay-observability.${input.format??'json'}`
  return {blob:await response.blob(),filename,contentType:response.headers.get('Content-Type')??'application/octet-stream'}
}

function withProvider<T extends object>(input:T,provider?:CLIProvider):T&{provider?:CLIProvider}{
  return provider===undefined?input:{...input,provider}
}

function feedbackDiscussionBody(feedbackId:UUID,input:FeedbackDiscussionRequest,createRevision=false):RequirementDiscussionRequest{
  const body:RequirementDiscussionRequest={
    title:input.title,
    body:input.body,
    messages:input.messages,
    feedbackId,
    ...(createRevision?{createRevision:true}:{}),
  }
  return withProvider(body,input.provider)
}

async function getFeedbackReferences(target:FeedbackAssociationTarget):Promise<FeedbackReference[]>{
  switch(target.kind){
    case 'plan':return (await unwrap<PlanDetail>(getPlan({path:{planId:target.id}}))).feedback
    case 'task':return (await unwrap<TaskDetail>(getTask({path:{taskId:target.id}}))).feedback
    case 'checkpoint':return (await unwrap<CheckpointDetail>(getCheckpoint({path:{checkpointId:target.id}}))).feedback
  }
}

function summarizeProbe(response:CLIProbeResponse):CLIProbeSummary{
  // Keep the facade usable while the backend implementation moves from the
  // former single-provider response to the generated two-provider contract.
  if(!Array.isArray(response.results))return response as CLIProbeSummary
  const output=response.results.map(result=>{
    const label=result.provider==='codex'?'Codex CLI':'Claude CLI'
    const detail=result.output.trim()||result.error||`进程退出码：${result.exitCode??'未启动'}`
    return `${label}：${result.available?'可用':'不可用'}\n${detail}`
  }).join('\n\n')
  const failed=response.results.find(result=>!result.available)
  return {...response,output,exitCode:failed?.exitCode??(failed?-1:0)}
}

export const api={
  exchange:(token:string)=>unwrap(exchangeAccessToken({body:{token}})),
  directories:(path?:string)=>unwrap<DirectoryListing>(listDirectories({query:path?{path}:undefined})),
  projects:()=>unwrap<Project[]>(listProjects()),
  createProject:(input:Pick<Project,'name'|'description'|'workspacePath'>)=>unwrap<Project>(createProject({body:input})),
  updateProject:(project:Project)=>unwrap<Project>(updateProject({path:{projectId:project.id},body:{name:project.name,description:project.description,workspacePath:project.workspacePath,version:project.version}})),
  settings:(id:UUID)=>unwrap<ProjectSettings>(getProjectSettings({path:{projectId:id}})),
  updateSettings:(settings:ProjectSettings)=>unwrap<ProjectSettings>(updateProjectSettings({path:{projectId:settings.projectId},body:settings})),
  mcpConnection:()=>unwrap<McpConnectionInfo>(getMcpConnectionInfo()),
  diagnoseMcp:()=>unwrap<McpDiagnostic>(diagnoseMcp()),
  rotateMcpToken:()=>unwrap<{token:string}>(rotateMcpToken()),
  automation:(project:Project,enabled:boolean)=>unwrap<Project>(setProjectAutomation({path:{projectId:project.id,action:enabled?'start':'stop'},body:{version:project.version}})),
  intakes:(projectId:UUID)=>unwrap<Intake[]>(listIntakes({path:{projectId}})),
  createIntake:(projectId:UUID,input:IntakeInput)=>{const{provider,...body}=input;return unwrap(createIntake({path:{projectId},body:withProvider(body,provider)}))},
  createFeedback:(projectId:UUID,input:FeedbackCreateInput)=>{const{provider,...body}=input;return unwrap<FeedbackCreateResponse>(createFeedback({path:{projectId},body:withProvider(body,provider)}))},
  feedback:(projectId:UUID,feedbackId:UUID)=>unwrap<FeedbackContext>(getFeedbackContext({path:{projectId,feedbackId}})),
  discussRequirement:(projectId:UUID,input:RequirementDiscussionRequest)=>{const{provider,...body}=input;return unwrap<RequirementDiscussionResult>(discussRequirement({path:{projectId},body:withProvider(body,provider)}))},
  discussFeedback:(projectId:UUID,feedbackId:UUID,input:FeedbackDiscussionRequest)=>unwrap<RequirementDiscussionResult>(discussRequirement({path:{projectId},body:feedbackDiscussionBody(feedbackId,input)})),
  createFeedbackRevision:(projectId:UUID,feedbackId:UUID,input:FeedbackDiscussionRequest)=>unwrap<RequirementDiscussionResult>(discussRequirement({path:{projectId},body:feedbackDiscussionBody(feedbackId,input,true)})),
  updateIntake:(intake:Intake)=>unwrap<Intake>(updateIntake({path:{intakeId:intake.id},body:{title:intake.title,body:intake.body,status:intake.status,version:intake.version}})),
  generatePlan:(intake:Intake,provider?:CLIProvider)=>unwrap<AsyncResponse>(generatePlan({path:{intakeId:intake.id},body:withProvider({version:intake.version},provider)})),
  generateFeedbackRevisionPlan:(revision:Intake,provider?:CLIProvider)=>unwrap<AsyncResponse>(generatePlan({path:{intakeId:revision.id},body:withProvider({version:revision.version},provider)})),
  upload:(intakeId:UUID,file:File)=>unwrap(uploadAttachment({path:{intakeId},body:{file}})),
  plans:(projectId:UUID)=>unwrap<Plan[]>(listPlans({path:{projectId}})),
  plan:(id:UUID)=>unwrap<PlanDetail>(getPlan({path:{planId:id}})),
  planExecutionContext:(id:UUID,provider?:CLIProvider)=>unwrap<PlanExecutionContext>(getPlanExecutionContext({path:{planId:id},query:provider===undefined?undefined:{provider}})),
  acceptPlanExecutionContext:(id:UUID,input:{baselineSnapshotId:UUID;fingerprint:string;reason:string;provider?:CLIProvider})=>unwrap(acceptPlanExecutionContext({path:{planId:id},body:input})),
  task:(id:UUID)=>unwrap<TaskDetail>(getTask({path:{taskId:id}})),
  checkpointDiff:(id:UUID)=>unwrap<CheckpointDetail>(getCheckpoint({path:{checkpointId:id}})),
  feedbackReferences:(target:FeedbackAssociationTarget)=>getFeedbackReferences(target),
  runPlan:(plan:Plan,provider?:CLIProvider)=>unwrap<AsyncResponse>(runPlan({path:{planId:plan.id},body:withProvider({version:plan.version},provider)})),
  runTask:(task:PlanTask,provider?:CLIProvider)=>unwrap<AsyncResponse>(runTask({path:{taskId:task.id,action:task.status==='failed'?'retry':'run'},body:withProvider({version:task.version},provider)})),
  events:(projectId:UUID,before?:number,limit?:number)=>unwrap<EventPage>(listEvents({query:{projectId,before,limit}})),
  agentRuns:(projectId:UUID,limit=100)=>getJSON<AgentRun[]>(`/api/v1/projects/${encodeURIComponent(projectId)}/agent-runs?limit=${limit}`),
  observability:(projectId:UUID,query?:ObservabilityQuery)=>unwrap<AgentRunObservabilityResponse>(queryProjectObservability({path:{projectId},query})),
  exportObservability:(projectId:UUID,input:ObservabilityExportRequest={})=>downloadObservability(projectId,input),
  agentRunLog:(runId:UUID,before?:number,limit=50)=>{const query=new URLSearchParams({limit:String(limit)});if(before!==undefined)query.set('before',String(before));return getJSON<AgentRunLog>(`/api/v1/agent-runs/${encodeURIComponent(runId)}/log?${query}`)},
  probe:(projectId:UUID)=>unwrap<CLIProbeResponse>(probeAgent({body:{projectId}})).then(summarizeProbe),
}
