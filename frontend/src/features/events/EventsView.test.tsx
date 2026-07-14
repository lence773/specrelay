// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { EventRecord } from '../../api/types'

const {eventsApi,subscribeApi}=vi.hoisted(()=>({eventsApi:vi.fn(),subscribeApi:vi.fn()}))
vi.mock('../../api/client',()=>({api:{events:eventsApi}}))
vi.mock('../../api/events',()=>({subscribe:subscribeApi}))

import { EventsView } from './EventsView'

const PROJECT_ID='11111111-1111-4111-8111-111111111111'
let emitLive:(event:EventRecord)=>void=()=>undefined

function event(id:number,overrides:Partial<EventRecord>={}):EventRecord{
  return {
    id,
    projectId:PROJECT_ID,
    type:'task.completed',
    aggregateType:'task',
    aggregateId:`event-${String(id).padStart(8,'0')}`,
    resourceVersion:id,
    payload:{},
    occurredAt:`2026-07-14T00:${String(id%60).padStart(2,'0')}:00Z`,
    ...overrides,
  }
}

function range(from:number,to:number){
  const result:EventRecord[]=[]
  for(let id=from;id>=to;id--)result.push(event(id))
  return result
}

function renderView(events:EventRecord[],projectId?:string){
  const queryClient=new QueryClient({defaultOptions:{queries:{retry:false}}})
  return render(<QueryClientProvider client={queryClient}><EventsView events={events} projectId={projectId}/></QueryClientProvider>)
}

function displayedIds(container:HTMLElement){
  return [...container.querySelectorAll<HTMLElement>('.event-row')].map(row=>Number(row.dataset.eventId))
}

beforeEach(()=>{
  eventsApi.mockReset()
  subscribeApi.mockReset()
  subscribeApi.mockImplementation((...args:unknown[])=>{
    emitLive=args[3] as (event:EventRecord)=>void
    return vi.fn()
  })
})

afterEach(()=>cleanup())

describe('EventsView',()=>{
  it('shows the newest page first with exactly 10 visible events and filters agent.output input',()=>{
    const {container}=renderView([event(100,{type:'agent.output'}),...range(15,1)])

    expect(displayedIds(container)).toEqual(range(15,6).map(item=>item.id))
    expect(screen.getByText('第 1 页')).toBeInTheDocument()
    expect(screen.getByText('最新事件')).toBeInTheDocument()
    expect(screen.getByRole('button',{name:'上一页'})).toBeDisabled()
    expect(screen.getByRole('button',{name:'下一页'})).toBeEnabled()
    expect(subscribeApi).toHaveBeenCalledWith(PROJECT_ID,15,expect.anything(),expect.any(Function))
  })

  it('loads older pages from nextBefore and restores the adjacent newer historical page from its stack',async()=>{
    eventsApi
      .mockResolvedValueOnce({items:range(20,11),hasMore:true,nextBefore:11})
      .mockResolvedValueOnce({items:range(10,1),hasMore:false,nextBefore:null})
    const {container}=renderView(range(30,20))

    fireEvent.click(screen.getByRole('button',{name:'下一页'}))
    await waitFor(()=>expect(screen.getByText('第 2 页')).toBeInTheDocument())
    expect(eventsApi).toHaveBeenNthCalledWith(1,PROJECT_ID,21,10)
    expect(displayedIds(container)).toEqual(range(20,11).map(item=>item.id))

    fireEvent.click(screen.getByRole('button',{name:'下一页'}))
    await waitFor(()=>expect(screen.getByText('第 3 页')).toBeInTheDocument())
    expect(eventsApi).toHaveBeenNthCalledWith(2,PROJECT_ID,11,10)
    expect(displayedIds(container)).toEqual(range(10,1).map(item=>item.id))

    fireEvent.click(screen.getByRole('button',{name:'上一页'}))
    expect(screen.getByText('第 2 页')).toBeInTheDocument()
    expect(displayedIds(container)).toEqual(range(20,11).map(item=>item.id))
    expect(eventsApi).toHaveBeenCalledTimes(2)
  })

  it('inserts unique live events at the top of the latest page, caps it at 10, and moves the history cursor to the new tail',async()=>{
    eventsApi.mockResolvedValueOnce({items:range(11,2),hasMore:true,nextBefore:2})
    const {container}=renderView(range(20,10))

    act(()=>{
      emitLive(event(21))
      emitLive(event(21))
    })
    expect(displayedIds(container)).toEqual(range(21,12).map(item=>item.id))

    fireEvent.click(screen.getByRole('button',{name:'下一页'}))
    await waitFor(()=>expect(eventsApi).toHaveBeenCalledWith(PROJECT_ID,12,10))
  })

  it('freezes a historical page, shows a new-event notice, and merges buffered events when returning latest',async()=>{
    eventsApi
      .mockResolvedValueOnce({items:range(20,11),hasMore:true,nextBefore:11})
      .mockResolvedValueOnce({items:range(30,21),hasMore:true,nextBefore:21})
    const {container}=renderView(range(30,20))

    fireEvent.click(screen.getByRole('button',{name:'下一页'}))
    await waitFor(()=>expect(screen.getByText('第 2 页')).toBeInTheDocument())
    const frozen=displayedIds(container)

    act(()=>emitLive(event(31)))
    expect(displayedIds(container)).toEqual(frozen)
    expect(screen.getByText('第 2 页')).toBeInTheDocument()
    const notice=screen.getByRole('status')
    expect(notice).toHaveTextContent('有新事件到达')

    fireEvent.click(within(notice).getByRole('button',{name:'返回最新页'}))
    await waitFor(()=>expect(screen.getByText('第 1 页')).toBeInTheDocument())
    expect(eventsApi).toHaveBeenLastCalledWith(PROJECT_ID,undefined,10)
    expect(displayedIds(container)).toEqual(range(31,22).map(item=>item.id))
    expect(screen.queryByText(/有新事件到达/)).not.toBeInTheDocument()
  })

  it('defensively filters agent.output from fetched pages, SSE, and the latest-page resync',async()=>{
    eventsApi
      .mockResolvedValueOnce({items:[event(50,{type:'agent.output'}),...range(10,2)],hasMore:false,nextBefore:null})
      .mockResolvedValueOnce({items:[event(99,{type:'agent.output'}),...range(20,12)],hasMore:false,nextBefore:null})
    const {container}=renderView([event(60,{type:'agent.output'}),...range(20,10)],PROJECT_ID)

    expect(displayedIds(container)).not.toContain(60)
    fireEvent.click(screen.getByRole('button',{name:'下一页'}))
    await waitFor(()=>expect(screen.getByText('第 2 页')).toBeInTheDocument())
    expect(displayedIds(container)).toEqual(range(10,2).map(item=>item.id))
    expect(displayedIds(container)).not.toContain(50)

    act(()=>emitLive(event(61,{type:'agent.output'})))
    expect(screen.queryByText(/有新事件到达/)).not.toBeInTheDocument()

    act(()=>emitLive(event(21)))
    const notice=screen.getByRole('status')
    fireEvent.click(within(notice).getByRole('button',{name:'返回最新页'}))
    await waitFor(()=>expect(screen.getByText('第 1 页')).toBeInTheDocument())
    expect(displayedIds(container)).toEqual(range(21,12).map(item=>item.id))
    expect(displayedIds(container)).not.toContain(99)
  })
})
