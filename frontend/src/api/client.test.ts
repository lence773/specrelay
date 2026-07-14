// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { client } from './generated/client.gen'
import type { CLIProvider, Intake, Plan, PlanTask, Project } from './types'

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
