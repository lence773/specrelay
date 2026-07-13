// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { client } from './generated/client.gen'
import type { Project } from './types'

function jsonResponse(body:unknown,status=200){return new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json','X-Request-ID':'request-header'}})}

function project(overrides:Partial<Project>={}):Project{return {id:'11111111-1111-4111-8111-111111111111',name:'SpecRelay',description:'',workspacePath:'/workspaces/specrelay',automationEnabled:false,createdAt:'2026-07-13T00:00:00Z',updatedAt:'2026-07-13T00:00:00Z',version:7,...overrides}}

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

  it('creates an intake through the generated operation',async()=>{
    const fetchMock=vi.fn(async(request:Request)=>{expect(new URL(request.url).pathname).toBe('/api/v1/projects/11111111-1111-4111-8111-111111111111/intakes');expect(await request.clone().json()).toEqual({kind:'requirement',title:'Contract first',body:'Use generated SDK'});return jsonResponse({intake:{id:'22222222-2222-4222-8222-222222222222'},job:null},201)})
    vi.stubGlobal('fetch',fetchMock)
    const result=await api.createIntake(project().id,{kind:'requirement',title:'Contract first',body:'Use generated SDK'})
    expect(result.intake.id).toBe('22222222-2222-4222-8222-222222222222')
  })

  it('discusses a requirement through the configured local CLI',async()=>{
    const input={title:'',body:'',messages:[{role:'user' as const,content:'希望先讨论清楚需求'}]}
    const fetchMock=vi.fn(async(request:Request)=>{
      expect(new URL(request.url).pathname).toBe('/api/v1/projects/11111111-1111-4111-8111-111111111111/intakes/discuss')
      expect(request.method).toBe('POST')
      expect(await request.clone().json()).toEqual(input)
      return jsonResponse({provider:'codex',reply:'请确认目标用户。',title:'需求讨论',body:'## 背景与目标',ready:false})
    })
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.discussRequirement(project().id,input)).resolves.toMatchObject({provider:'codex',ready:false})
    expect(fetchMock).toHaveBeenCalledOnce()
  })

  it('probes only by project id',async()=>{
    const fetchMock=vi.fn(async(request:Request)=>{expect(new URL(request.url).pathname).toBe('/api/v1/agents/probe');expect(await request.clone().json()).toEqual({projectId:project().id});return jsonResponse({provider:'codex',output:'codex 1.0',exitCode:0})})
    vi.stubGlobal('fetch',fetchMock)
    await expect(api.probe(project().id)).resolves.toMatchObject({provider:'codex',exitCode:0})
  })
})
