import { client } from './generated/client.gen'
import {
  createIntake,
  createProject,
  discussRequirement,
  exchangeAccessToken,
  generatePlan,
  getPlan,
  getProjectSettings,
  listDirectories,
  listEvents,
  listIntakes,
  listPlans,
  listProjects,
  probeAgent,
  runPlan,
  runTask,
  setProjectAutomation,
  updateIntake,
  updateProject,
  updateProjectSettings,
  uploadAttachment,
} from './generated/sdk.gen'
import type {
  APIError,
  AgentRun,
  AgentRunLog,
  CLIProbeResponse,
  CLIProbeSummary,
  CLIProvider,
  DirectoryListing,
  AsyncResponse,
  EventRecord,
  Intake,
  IntakeInput,
  Plan,
  PlanTask,
  Project,
  ProjectSettings,
  RequirementDiscussionRequest,
  RequirementDiscussionResult,
  UUID,
} from './types'

client.setConfig({baseUrl:'/api/v1',credentials:'same-origin'})

export class ClientError extends Error {
  constructor(public status:number,public body:APIError){super(body.message);this.name='ClientError'}
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
  automation:(project:Project,enabled:boolean)=>unwrap<Project>(setProjectAutomation({path:{projectId:project.id,action:enabled?'start':'stop'},body:{version:project.version}})),
  intakes:(projectId:UUID)=>unwrap<Intake[]>(listIntakes({path:{projectId}})),
  createIntake:(projectId:UUID,input:IntakeInput)=>unwrap(createIntake({path:{projectId},body:input})),
  discussRequirement:(projectId:UUID,input:RequirementDiscussionRequest)=>unwrap<RequirementDiscussionResult>(discussRequirement({path:{projectId},body:input})),
  updateIntake:(intake:Intake)=>unwrap<Intake>(updateIntake({path:{intakeId:intake.id},body:{title:intake.title,body:intake.body,status:intake.status,version:intake.version}})),
  generatePlan:(intake:Intake,provider?:CLIProvider)=>unwrap<AsyncResponse>(generatePlan({path:{intakeId:intake.id},body:provider===undefined?{version:intake.version}:{version:intake.version,provider}})),
  upload:(intakeId:UUID,file:File)=>unwrap(uploadAttachment({path:{intakeId},body:{file}})),
  plans:(projectId:UUID)=>unwrap<Plan[]>(listPlans({path:{projectId}})),
  plan:(id:UUID)=>unwrap<{plan:Plan;tasks:PlanTask[]}>(getPlan({path:{planId:id}})),
  runPlan:(plan:Plan,provider?:CLIProvider)=>unwrap<AsyncResponse>(runPlan({path:{planId:plan.id},body:provider===undefined?{version:plan.version}:{version:plan.version,provider}})),
  runTask:(task:PlanTask,provider?:CLIProvider)=>unwrap<AsyncResponse>(runTask({path:{taskId:task.id,action:task.status==='failed'?'retry':'run'},body:provider===undefined?{version:task.version}:{version:task.version,provider}})),
  events:(projectId:UUID,after=0)=>unwrap<EventRecord[]>(listEvents({query:{projectId,after}})),
  agentRuns:(projectId:UUID,limit=100)=>getJSON<AgentRun[]>(`/api/v1/projects/${encodeURIComponent(projectId)}/agent-runs?limit=${limit}`),
  agentRunLog:(runId:UUID,before?:number,limit=50)=>{const query=new URLSearchParams({limit:String(limit)});if(before!==undefined)query.set('before',String(before));return getJSON<AgentRunLog>(`/api/v1/agent-runs/${encodeURIComponent(runId)}/log?${query}`)},
  probe:(projectId:UUID)=>unwrap<CLIProbeResponse>(probeAgent({body:{projectId}})).then(summarizeProbe),
}
