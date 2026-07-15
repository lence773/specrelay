// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../../api/client'
import type { AgentRunObservabilityResponse, Plan, Project } from '../../api/types'
import { RunsView } from './RunsView'

const project:Project={
  id:'11111111-1111-4111-8111-111111111111',name:'Private project',description:'',workspacePath:'/private/workspace',
  automationEnabled:false,createdAt:'2026-07-01T00:00:00Z',updatedAt:'2026-07-01T00:00:00Z',version:1,
}
const plan:Plan={
  id:'22222222-2222-4222-8222-222222222222',projectId:project.id,intakeId:'33333333-3333-4333-8333-333333333333',title:'Observability plan',
  spec:{title:'Observability plan',summary:'Regression coverage',tasks:[],finalValidation:['tests pass']},markdown:'# Plan',status:'ready',
  createdAt:'2026-07-01T00:00:00Z',updatedAt:'2026-07-01T00:00:00Z',version:1,
}

function unavailableUsage(totalRunCount:number){
  return {
    tokens:{available:false,coverageCount:0,totalRunCount},
    costs:{available:false,coverageCount:0,totalRunCount,currencies:[]},
  }
}

function observabilityResponse():AgentRunObservabilityResponse{
  return {
    projectId:project.id,filter:{},pagination:{page:1,pageSize:200,totalItems:250,hasMore:true},
    requirements:[{id:plan.intakeId,title:'Private requirement',status:'open'}],
    plans:[{id:plan.id,requirementId:plan.intakeId,title:plan.title,status:'running'}],
    tasks:[{id:'44444444-4444-4444-8444-444444444444',planId:plan.id,taskKey:'P005',title:'Regression task',status:'succeeded'}],
    runs:[{
      id:'55555555-5555-4555-8555-555555555555',requirementId:plan.intakeId,planId:plan.id,taskId:'44444444-4444-4444-8444-444444444444',
      logicalOperationId:'66666666-6666-4666-8666-666666666666',operationType:'task_execution',jobAttempt:2,retryCount:1,provider:'codex',
      sessionMode:'snapshot_restored',status:'succeeded',queueWaitMs:100,durationMs:2500,outputLines:50,startedAt:'2026-07-15T01:00:00Z',finishedAt:'2026-07-15T01:00:02Z',
    }],
    aggregates:{
      sessionReuseRate:{available:true,numerator:1,denominator:2,value:.5},snapshotRestoreRate:{available:true,numerator:1,denominator:2,value:.5},
      planGenerationSuccessRate:{available:false,numerator:0,denominator:0},taskExecutionSuccessRate:{available:true,numerator:1,denominator:1,value:1},
      failureCategories:[],durationTrend:[{bucket:'2026-07-15',runCount:1,queueWait:{available:true,coverageCount:1,totalMs:100,averageMs:100},runDuration:{available:true,coverageCount:1,totalMs:2500,averageMs:2500}}],
      usage:{overall:unavailableUsage(250),byProvider:[],byRequirement:[],byPlan:[]},
    },
  }
}

function renderRunsView(){
  const queryClient=new QueryClient({defaultOptions:{queries:{retry:false}}})
  const rendered=render(<QueryClientProvider client={queryClient}><RunsView project={project} plans={[plan]}/></QueryClientProvider>)
  return {...rendered,queryClient}
}

beforeEach(()=>{
  vi.stubGlobal('requestAnimationFrame',(callback:FrameRequestCallback)=>{callback(0);return 1})
  vi.stubGlobal('cancelAnimationFrame',vi.fn())
  class MockURL extends URL {}
  Object.defineProperties(MockURL,{
    createObjectURL:{value:vi.fn(()=> 'blob:export')},
    revokeObjectURL:{value:vi.fn()},
  })
  vi.stubGlobal('URL',MockURL)
  vi.spyOn(HTMLAnchorElement.prototype,'click').mockImplementation(()=>{})
})

afterEach(()=>{
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('RunsView observability regressions',()=>{
  it('couples filters, labels unavailable usage, guards export metadata, and lazily prepends older logs',async()=>{
    const observability=vi.spyOn(api,'observability').mockResolvedValue(observabilityResponse())
    vi.spyOn(api,'agentRuns').mockResolvedValue([{
      id:'55555555-5555-4555-8555-555555555555',projectId:project.id,provider:'codex',commandSummary:'structured task run',status:'succeeded',durationMs:2500,
      startedAt:'2026-07-15T01:00:00Z',finishedAt:'2026-07-15T01:00:02Z',createdAt:'2026-07-15T01:00:00Z',updatedAt:'2026-07-15T01:00:02Z',version:1,
      sessionInvalidationReason:'provider_switched',
    } as never])
    const log=vi.spyOn(api,'agentRunLog').mockImplementation(async(_runId,before,limit)=>({
      runId:'55555555-5555-4555-8555-555555555555',status:'succeeded',provider:'codex',sizeBytes:1000,
      lines:before===undefined?Array.from({length:50},(_,index)=>`latest event ${index+1}`):['older event 49','older event 50'],
      hasMore:before===undefined,nextBefore:before===undefined?500:undefined,updatedAt:'2026-07-15T01:00:02Z',
    }))
    const exportObservability=vi.spyOn(api,'exportObservability').mockResolvedValue({blob:new Blob(['{}'],{type:'application/json'}),filename:'observability.json',contentType:'application/json'})
    const {queryClient}=renderRunsView()

    expect(await screen.findByText('快照恢复')).toBeInTheDocument()
    expect((await screen.findAllByText('会话失效：Provider 已切换')).length).toBeGreaterThan(0)
    expect(screen.getAllByText('不可用').length).toBeGreaterThan(1)
    expect(screen.getAllByText('覆盖 0/250 次调用').length).toBeGreaterThanOrEqual(2)
    expect(screen.getByText('Token 与费用趋势覆盖当前列表的最近 1 次调用；聚合总量覆盖全部 250 次调用。')).toBeInTheDocument()
    await waitFor(()=>expect(log).toHaveBeenCalledWith('55555555-5555-4555-8555-555555555555',undefined,50))
    expect(screen.getByText('latest event 50')).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('Provider'),{target:{value:'claude'}})
    fireEvent.change(screen.getByLabelText('计划'),{target:{value:plan.id}})
    await waitFor(()=>{
      const latest=observability.mock.calls.at(-1)
      expect(latest?.[0]).toBe(project.id)
      expect(latest?.[1]).toMatchObject({provider:'claude',planId:plan.id,page:1,pageSize:200})
    })
    expect(screen.getByText(/Private project · 最近 7 天 · Claude CLI · Observability plan/)).toBeInTheDocument()

    fireEvent.click(screen.getByText('设置敏感元数据'))
    const workspaceOption=screen.getByLabelText('工作区路径')
    fireEvent.click(workspaceOption)
    const jsonButton=screen.getByRole('button',{name:'导出 JSON'})
    expect(jsonButton).toBeDisabled()
    fireEvent.click(screen.getByLabelText(/我确认所选字段可能包含敏感元数据/))
    expect(jsonButton).toBeEnabled()
    fireEvent.click(jsonButton)
    await waitFor(()=>expect(exportObservability).toHaveBeenCalledWith(project.id,expect.objectContaining({format:'json',provider:'claude',planId:plan.id,includeWorkspacePath:true,includeProjectName:false,includeBusinessTitles:false})))

    fireEvent.click(screen.getByRole('button',{name:'向上滚动或点击加载更早 50 条'}))
    await waitFor(()=>expect(log).toHaveBeenCalledWith('55555555-5555-4555-8555-555555555555',500,50))
    expect(await screen.findByText('older event 49')).toBeInTheDocument()
    expect(screen.getByText('latest event 1')).toBeInTheDocument()

    await act(async()=>{await queryClient.cancelQueries();queryClient.clear()})
  })
})
