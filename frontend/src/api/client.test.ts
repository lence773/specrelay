// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { client } from './generated/client.gen'
import type { CLIProvider, FeedbackDiscussionRequest, Intake, Plan, PlanTask, Project } from './types'

function jsonResponse(body:unknown,status=200){return new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json','X-Request-ID':'request-header'}})}

function project(overrides:Partial<Project>={}):Project{return {id:'11111111-1111-4111-8111-111111111111',name:'SpecRelay',description:'',workspacePath:'/workspaces/specrelay',automationEnabled:false,createdAt:'2026-07-13T00:00:00Z',updatedAt:'2026-07-13T00:00:00Z',version:7,...overrides}}
function intake(overrides:Partial<Intake>={}):Intake{return {id:'22222222-2222-4222-8222-222222222222',projectId:project().id,kind:'requirement',title:'Contract first',body:'Use generated SDK',status:'open',configSnapshot:{},createdAt:'2026-07-13T00:00:00Z',updatedAt:'2026-07-13T00:00:00Z',version:11,...overrides}}
function plan(overrides:Partial<Plan>={}):Plan{return {id:'33333333-3333-4333-8333-333333333333',projectId:project().id,intakeId:intake().id,title:'Provider plan',spec:{title:'Provider plan',summary:'Exercise provider selection',tasks:[],finalValidation:['tests pass']},markdown:'# Provider plan',status:'ready',createdAt:'2026-07-13T00:00:00Z',updatedAt:'2026-07-13T00:00:00Z',version:13,...overrides}}
function task(overrides:Partial<PlanTask>={}):PlanTask{return {id:'44444444-4444-4444-8444-444444444444',projectId:project().id,planId:plan().id,taskKey:'P001',position:1,title:'Implement',scope:['frontend'],acceptance:['passes'],status:'pending',createdAt:'2026-07-13T00:00:00Z',updatedAt:'2026-07-13T00:00:00Z',version:17,...overrides}}

beforeEach(()=>client.setConfig({baseUrl:'http://localhost:3000/api/v1',credentials:'same-origin'}))
afterEach(()=>{vi.unstubAllGlobals();client.setConfig({baseUrl:'/api/v1',credentials:'same-origin'})})

describe('generated API facade',()=>{
  it('exchanges the bootstrap token in a JSON body with same-origin credentials',async()=>{
    const fetchMock=vi.fn(async(request:Request)=>{expect(request.url).toBe('http://localhost:3000/api/v1/auth/exchange');expect(request.credentials).toBe('same-origin');expect(request.method).toBe('POST');expect(await request.clone().json()).toEqual({token:'one time token'});return jsonResponse({state:'authenticated'})})
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.exchange('one time token')).resolves.toEqual({state:'authenticated'})
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('maps generated SDK errors to ClientError',async()=>{
    vi.stubGlobal('fetch',vi.fn(async()=>jsonResponse({code:'resource_version_conflict',message:'Version mismatch',details:{expected:8},requestId:'request-body'},409)))
    await expect(api.projects()).rejects.toMatchObject({status:409,body:{code:'resource_version_conflict',message:'Version mismatch',requestId:'request-body'}})
  })

  it('sends the current resource version when changing automation state',async()=>{
    const fetchMock=vi.fn(async(request:Request)=>{expect(new URL(request.url).pathname).toBe('/api/v1/projects/11111111-1111-4111-8111-111111111111/automation/start');expect(await request.clone().json()).toEqual({version:7});return jsonResponse(project({automationEnabled:true,version:8}))})
    vi.stubGlobal('fetch',fetchMock)
    await api.automation(project(),true)
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('fetches an event page with project, cursor, and limit metadata intact',async()=>{
    const page={
      items:[{id:42,projectId:project().id,type:'task.completed',aggregateType:'task',aggregateId:'44444444-4444-4444-8444-444444444444',resourceVersion:3,payload:{},occurredAt:'2026-07-13T00:00:00Z'}],
      hasMore:true,
      nextBefore:42,
    }
    const fetchMock=vi.fn(async(request:Request)=>{
      const url=new URL(request.url)
      expect(url.pathname).toBe('/api/v1/events')
      expect(Object.fromEntries(url.searchParams)).toEqual({projectId:project().id,before:'77',limit:'25'})
      return jsonResponse(page)
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.events(project().id,77,25)).resolves.toEqual(page)
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('creates an intake through the generated operation',async()=>{
    const fetchMock=vi.fn(async(request:Request)=>{expect(new URL(request.url).pathname).toBe('/api/v1/projects/11111111-1111-4111-8111-111111111111/intakes');expect(await request.clone().json()).toEqual({kind:'requirement',title:'Contract first',body:'Use generated SDK'});return jsonResponse({intake:{id:'22222222-2222-4222-8222-222222222222'},job:null},201)})
    vi.stubGlobal('fetch',fetchMock)
    const result=await api.createIntake(project().id,{kind:'requirement',title:'Contract first',body:'Use generated SDK'})
    expect(result.intake.id).toBe('22222222-2222-4222-8222-222222222222')
  })

  it('omits an unselected provider and sends explicit Codex or Claude selections for every execution entry',async()=>{
    const discussion={title:'',body:'',messages:[{role:'user' as const,content:'希望先讨论清楚需求'}]}
    const entries:Array<{path:string;baseBody:Record<string,unknown>;invoke:(provider?:CLIProvider)=>Promise<unknown>}>= [
      {path:`/api/v1/projects/${project().id}/intakes/discuss`,baseBody:discussion,invoke:provider=>api.discussRequirement(project().id,provider===undefined?discussion:{...discussion,provider})},
      {path:`/api/v1/intakes/${intake().id}/generate`,baseBody:{version:intake().version},invoke:provider=>api.generatePlan(intake(),provider)},
      {path:`/api/v1/plans/${plan().id}/run`,baseBody:{version:plan().version},invoke:provider=>api.runPlan(plan(),provider)},
      {path:`/api/v1/tasks/${task().id}/run`,baseBody:{version:task().version},invoke:provider=>api.runTask(task(),provider)},
      {path:`/api/v1/tasks/${task().id}/retry`,baseBody:{version:task().version},invoke:provider=>api.runTask(task({status:'failed'}),provider)},
    ]
    const expected=entries.flatMap(entry=>([undefined,'codex','claude'] as const).map(provider=>({entry,provider})))
    const fetchMock=vi.fn(async(request:Request)=>{
      const next=expected.shift()
      expect(next).toBeDefined()
      expect(new URL(request.url).pathname).toBe(next!.entry.path)
      expect(request.method).toBe('POST')
      expect(await request.clone().json()).toEqual(next!.provider===undefined?next!.entry.baseBody:{...next!.entry.baseBody,provider:next!.provider})
      return next!.entry.path.endsWith('/discuss')
        ?jsonResponse({provider:next!.provider??'codex',reply:'请确认目标用户。',title:'需求讨论',body:'## 背景与目标',ready:false})
        :jsonResponse({jobId:'55555555-5555-4555-8555-555555555555',state:'queued',resourceVersion:1})
    })
    vi.stubGlobal('fetch',fetchMock)
    for(const entry of entries){
      await entry.invoke()
      await entry.invoke('codex')
      await entry.invoke('claude')
    }
    expect(expected).toHaveLength(0)
    expect(fetchMock).toHaveBeenCalledTimes(entries.length*3)
  })

  it('serializes the complete feedback association without inventing a provider',async()=>{
    const association={
      requirementId:intake().id,
      title:'Diff feedback',
      body:'Please adjust these lines.',
      planId:plan().id,
      taskId:task().id,
      checkpointId:'55555555-5555-4555-8555-555555555555',
      fileId:'66666666-6666-4666-8666-666666666666',
      diffHunkId:'77777777-7777-4777-8777-777777777777',
      diffLineSide:'new' as const,
      diffLineStart:7,
      diffLineEnd:9,
    }
    const fetchMock=vi.fn(async(request:Request)=>{
      expect(new URL(request.url).pathname).toBe(`/api/v1/projects/${project().id}/feedback`)
      expect(request.method).toBe('POST')
      expect(await request.clone().json()).toEqual(association)
      return jsonResponse({feedback:{...intake(),kind:'feedback',parentIntakeId:intake().id,title:association.title,body:association.body},job:null},201)
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.createFeedback(project().id,association)).resolves.toMatchObject({feedback:{kind:'feedback',title:'Diff feedback'},job:null})
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('returns checkpoint changed files, Diff line coordinates, and reverse feedback status',async()=>{
    const checkpointId='55555555-5555-4555-8555-555555555555'
    const fileId='66666666-6666-4666-8666-666666666666'
    const hunkId='77777777-7777-4777-8777-777777777777'
    const response={
      checkpoint:{
        id:checkpointId,projectId:project().id,planId:plan().id,intakeId:intake().id,taskId:task().id,sequence:4,kind:'task_checkpoint',changeSummary:{summary:'updated client'},additions:3,deletions:1,createdAt:'2026-07-15T01:00:00Z',
        files:[{id:fileId,snapshotId:checkpointId,sequence:1,path:'frontend/src/api/client.ts',status:'modified',staged:false,binary:false,additions:3,deletions:1,createdAt:'2026-07-15T01:00:00Z',hunks:[{id:hunkId,fileId,sequence:1,header:'@@ -4,2 +7,3 @@',patch:'+new',oldStartLine:4,oldLineCount:2,newStartLine:7,newLineCount:3,createdAt:'2026-07-15T01:00:00Z'}]}],
      },
      feedback:[{id:'88888888-8888-4888-8888-888888888888',requirementId:intake().id,title:'Review Diff',feedbackStatus:'open',revisionStatus:'planning',createdAt:'2026-07-15T02:00:00Z'}],
    }
    const fetchMock=vi.fn(async(request:Request)=>{
      expect(new URL(request.url).pathname).toBe(`/api/v1/checkpoints/${checkpointId}`)
      return jsonResponse(response)
    })
    vi.stubGlobal('fetch',fetchMock)
    const result=await api.checkpointDiff(checkpointId)
    expect(result.checkpoint.files[0].hunks[0]).toMatchObject({oldStartLine:4,oldLineCount:2,newStartLine:7,newLineCount:3})
    expect(result.feedback[0]).toMatchObject({revisionStatus:'planning'})
  })

  it('loads bounded feedback context and reverse references for each association target',async()=>{
    const feedbackId='88888888-8888-4888-8888-888888888888'
    const checkpointId='55555555-5555-4555-8555-555555555555'
    const paths:string[]=[]
    const fetchMock=vi.fn(async(request:Request)=>{
      const path=new URL(request.url).pathname
      paths.push(path)
      if(path===`/api/v1/projects/${project().id}/feedback/${feedbackId}`)return jsonResponse({
        feedback:{id:feedbackId,title:'Review',body:'Fix range',status:'open',createdAt:'2026-07-15T00:00:00Z',updatedAt:'2026-07-15T00:00:00Z'},
        requirement:{id:intake().id,title:intake().title,body:intake().body,status:'open',createdAt:intake().createdAt,updatedAt:intake().updatedAt},
        association:{requirementId:intake().id,planId:plan().id,taskId:task().id,checkpointId,diffLineSide:'new',diffLineStart:7,diffLineEnd:9},
        revision:{currentStatus:'not_started',items:[]},
      })
      const reference={id:feedbackId,requirementId:intake().id,title:'Review',feedbackStatus:'open',revisionStatus:'not_started',createdAt:'2026-07-15T00:00:00Z'}
      if(path===`/api/v1/plans/${plan().id}`)return jsonResponse({plan:plan(),tasks:[task()],feedback:[reference]})
      if(path===`/api/v1/tasks/${task().id}`)return jsonResponse({task:task(),feedback:[reference]})
      return jsonResponse({checkpoint:{id:checkpointId,projectId:project().id,planId:plan().id,intakeId:intake().id,sequence:1,kind:'task_checkpoint',changeSummary:{},files:[],createdAt:'2026-07-15T00:00:00Z'},feedback:[reference]})
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.feedback(project().id,feedbackId)).resolves.toMatchObject({association:{diffLineStart:7,diffLineEnd:9},revision:{currentStatus:'not_started'}})
    await expect(api.feedbackReferences({kind:'plan',id:plan().id})).resolves.toHaveLength(1)
    await expect(api.feedbackReferences({kind:'task',id:task().id})).resolves.toHaveLength(1)
    await expect(api.feedbackReferences({kind:'checkpoint',id:checkpointId})).resolves.toHaveLength(1)
    expect(paths).toEqual([`/api/v1/projects/${project().id}/feedback/${feedbackId}`,`/api/v1/plans/${plan().id}`,`/api/v1/tasks/${task().id}`,`/api/v1/checkpoints/${checkpointId}`])
  })

  it('maps feedback relation errors to ClientError with the server request id',async()=>{
    vi.stubGlobal('fetch',vi.fn(async()=>jsonResponse({code:'invalid_diff_range',message:'Diff line range must be complete and contained in the selected hunk',requestId:'feedback-request'},400)))
    await expect(api.createFeedback(project().id,{requirementId:intake().id,title:'Bad range',body:'Outside hunk',diffHunkId:'77777777-7777-4777-8777-777777777777',diffLineSide:'new',diffLineStart:99,diffLineEnd:101})).rejects.toMatchObject({status:400,body:{code:'invalid_diff_range',requestId:'feedback-request'}})
  })

  it('keeps feedback discussions session-free and creates revisions only when explicitly requested',async()=>{
    const feedbackId='88888888-8888-4888-8888-888888888888'
    const unsafeInput:FeedbackDiscussionRequest&{sessionId:string;sessionProvider:CLIProvider}={title:'Revision',body:'Small fix',messages:[{role:'user',content:'Confirm the revision'}],provider:'claude',sessionId:'another-project-session',sessionProvider:'codex'}
    const bodies:unknown[]=[]
    const fetchMock=vi.fn(async(request:Request)=>{
      bodies.push(await request.clone().json())
      return jsonResponse({provider:'claude',reply:'Ready',title:'Revision',body:'Small fix',ready:true})
    })
    vi.stubGlobal('fetch',fetchMock)
    await api.discussFeedback(project().id,feedbackId,unsafeInput)
    await api.createFeedbackRevision(project().id,feedbackId,unsafeInput)
    expect(bodies).toEqual([
      {title:'Revision',body:'Small fix',messages:[{role:'user',content:'Confirm the revision'}],feedbackId,provider:'claude'},
      {title:'Revision',body:'Small fix',messages:[{role:'user',content:'Confirm the revision'}],feedbackId,createRevision:true,provider:'claude'},
    ])
  })

  it('generates a feedback revision plan with an optional provider override',async()=>{
    const revision=intake({id:'99999999-9999-4999-8999-999999999999',title:'Revision requirement',version:23})
    const bodies:unknown[]=[]
    const fetchMock=vi.fn(async(request:Request)=>{
      expect(new URL(request.url).pathname).toBe(`/api/v1/intakes/${revision.id}/generate`)
      bodies.push(await request.clone().json())
      return jsonResponse({jobId:'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',state:'queued',resourceVersion:1},202)
    })
    vi.stubGlobal('fetch',fetchMock)
    await api.generateFeedbackRevisionPlan(revision)
    await api.generateFeedbackRevisionPlan(revision,'codex')
    expect(bodies).toEqual([{version:23},{version:23,provider:'codex'}])
  })

  it('keeps observability filters and pagination coupled in the generated query',async()=>{
    const response={projectId:project().id,filter:{provider:'claude'},pagination:{page:2,pageSize:25,totalItems:26,hasMore:false},requirements:[],plans:[],tasks:[],runs:[],aggregates:{sessionReuseRate:{available:false,numerator:0,denominator:0},snapshotRestoreRate:{available:false,numerator:0,denominator:0},planGenerationSuccessRate:{available:false,numerator:0,denominator:0},taskExecutionSuccessRate:{available:false,numerator:0,denominator:0},failureCategories:[],durationTrend:[],usage:{overall:{tokens:{available:false,coverageCount:0,totalRunCount:0},costs:{available:false,coverageCount:0,totalRunCount:0,currencies:[]}},byProvider:[],byRequirement:[],byPlan:[]}}}
    const fetchMock=vi.fn(async(request:Request)=>{
      const url=new URL(request.url)
      expect(url.pathname).toBe(`/api/v1/projects/${project().id}/observability`)
      expect(Object.fromEntries(url.searchParams)).toEqual({from:'2026-07-01T00:00:00Z',to:'2026-07-15T00:00:00Z',provider:'claude',planId:plan().id,page:'2',pageSize:'25'})
      return jsonResponse(response)
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.observability(project().id,{from:'2026-07-01T00:00:00Z',to:'2026-07-15T00:00:00Z',provider:'claude',planId:plan().id,page:2,pageSize:25})).resolves.toMatchObject({pagination:{page:2,pageSize:25,totalItems:26}})
  })

  it('downloads redacted exports by default and sends only explicitly selected metadata options',async()=>{
    const requests:Array<Record<string,string>>=[]
    const fetchMock=vi.fn(async(input:RequestInfo|URL)=>{
      const url=new URL(typeof input==='string'?input:input instanceof URL?input.href:input.url,'http://localhost:3000')
      requests.push(Object.fromEntries(url.searchParams))
      const first=requests.length===1
      return new Response(first?'{}':'section,key\nmetadata,ok\n',{status:200,headers:first?{'Content-Type':'application/json'}:{'Content-Type':'text/csv','Content-Disposition':'attachment; filename="safe-export.csv"'}})
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.exportObservability(project().id,{format:'json'})).resolves.toMatchObject({filename:'specrelay-observability.json',contentType:'application/json'})
    await expect(api.exportObservability(project().id,{format:'csv',includeProjectName:true,includeWorkspacePath:false,includeBusinessTitles:true,provider:'codex',planId:plan().id})).resolves.toMatchObject({filename:'safe-export.csv',contentType:'text/csv'})
    expect(requests[0]).toEqual({format:'json'})
    expect(requests[1]).toEqual({format:'csv',includeProjectName:'true',includeWorkspacePath:'false',includeBusinessTitles:'true',provider:'codex',planId:plan().id})
  })

  it('requests the latest 50 log lines first and uses the exclusive cursor for older pages',async()=>{
    const seen:string[]=[]
    const fetchMock=vi.fn(async(input:RequestInfo|URL)=>{
      const url=new URL(typeof input==='string'?input:input instanceof URL?input.href:input.url,'http://localhost:3000')
      seen.push(url.pathname+url.search)
      const before=url.searchParams.get('before')
      return jsonResponse({runId:'run-1',status:'succeeded',provider:'codex',lines:before?['older']:['latest'],sizeBytes:100,hasMore:!before,nextBefore:before?undefined:500,updatedAt:'2026-07-15T00:00:00Z'})
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.agentRunLog('run-1')).resolves.toMatchObject({lines:['latest'],hasMore:true,nextBefore:500})
    await expect(api.agentRunLog('run-1',500,50)).resolves.toMatchObject({lines:['older'],hasMore:false})
    expect(seen).toEqual(['/api/v1/agent-runs/run-1/log?limit=50','/api/v1/agent-runs/run-1/log?limit=50&before=500'])
  })

  it('probes both CLIs using only the project id without selecting a business default',async()=>{
    const response={results:[
      {provider:'codex' as const,available:true,output:'codex 1.0',exitCode:0,error:null},
      {provider:'claude' as const,available:true,output:'claude 2.0',exitCode:0,error:null},
    ] as const}
    const fetchMock=vi.fn(async(request:Request)=>{
      expect(new URL(request.url).pathname).toBe('/api/v1/agents/probe')
      expect(await request.clone().json()).toEqual({projectId:project().id})
      return jsonResponse(response)
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.probe(project().id)).resolves.toMatchObject({results:response.results,exitCode:0})
    expect(fetchMock).toHaveBeenCalledOnce()
  })
})
