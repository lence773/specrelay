// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { QueryClient } from '@tanstack/react-query'
import { subscribe } from './events'
import type { EventRecord } from './types'

class FakeEventSource {
  static latest:FakeEventSource
  onmessage:((event:MessageEvent<string>)=>void)|null=null
  close=vi.fn()
  constructor(public url:string){FakeEventSource.latest=this}
  emit(event:EventRecord){this.onmessage?.(new MessageEvent('message',{data:JSON.stringify(event)}))}
}

function event(overrides:Partial<EventRecord>={}):EventRecord {
  return {id:42,projectId:'project/id',type:'task.retry_wait',aggregateType:'task',aggregateId:'task-id',resourceVersion:3,payload:{tail:'retrying'},occurredAt:'2026-07-13T00:00:00Z',...overrides}
}

function setup(after=41){
  vi.stubGlobal('EventSource',FakeEventSource)
  const invalidateQueries=vi.fn().mockResolvedValue(undefined)
  const queryClient={invalidateQueries} as unknown as QueryClient
  const onEvent=vi.fn()
  const unsubscribe=subscribe('project/id',after,queryClient,onEvent)
  return {invalidateQueries,onEvent,unsubscribe}
}

afterEach(()=>vi.unstubAllGlobals())

describe('event subscription',()=>{
  it('starts strictly after the latest visible event and refreshes task-related caches without invalidating event pages',()=>{
    const {invalidateQueries,onEvent,unsubscribe}=setup()
    expect(FakeEventSource.latest.url).toBe('/api/v1/events/stream?projectId=project%2Fid&after=41')
    const next=event()
    FakeEventSource.latest.emit(next)
    expect(onEvent).toHaveBeenCalledWith(next)
    expect(invalidateQueries).toHaveBeenCalledWith({queryKey:['plans','project/id']})
    expect(invalidateQueries).not.toHaveBeenCalledWith({queryKey:['events','project/id']})
    expect(invalidateQueries).not.toHaveBeenCalledWith({queryKey:['projects']})
    unsubscribe()
    expect(FakeEventSource.latest.close).toHaveBeenCalledOnce()
  })

  it('deduplicates event IDs and defensively ignores agent.output messages',()=>{
    const {invalidateQueries,onEvent}=setup()
    const next=event()
    FakeEventSource.latest.emit(next)
    FakeEventSource.latest.emit(next)
    FakeEventSource.latest.emit(event({id:43,type:'agent.output',aggregateType:'agent_run',aggregateId:'run-id'}))
    expect(onEvent).toHaveBeenCalledTimes(1)
    expect(onEvent).toHaveBeenCalledWith(next)
    expect(invalidateQueries).toHaveBeenCalledTimes(1)
  })

  it.each([
    ['project',event({aggregateType:'project',aggregateId:'project/id'}),[['projects']]],
    ['intake',event({aggregateType:'intake',aggregateId:'intake-id'}),[['intakes','project/id']]],
    ['plan',event({aggregateType:'plan',aggregateId:'plan-id'}),[['plans','project/id']]],
    ['task',event({aggregateType:'task',aggregateId:'task-id'}),[['plans','project/id']]],
    ['agent run',event({aggregateType:'agent_run',aggregateId:'run-id'}),[['agent-runs','project/id'],['agent-run-log','run-id']]],
  ])('refreshes %s caches according to aggregate type',(_label,next,expectedKeys)=>{
    const {invalidateQueries}=setup()
    FakeEventSource.latest.emit(next)
    expect(invalidateQueries.mock.calls.map(([options])=>options.queryKey)).toEqual(expectedKeys)
  })
})
