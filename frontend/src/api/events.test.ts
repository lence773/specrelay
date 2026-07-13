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

afterEach(()=>vi.unstubAllGlobals())

describe('event subscription',()=>{
  it('handles every domain event through the standard message channel and invalidates related caches',()=>{
    vi.stubGlobal('EventSource',FakeEventSource)
    const invalidateQueries=vi.fn().mockResolvedValue(undefined)
    const queryClient={invalidateQueries} as unknown as QueryClient
    const onEvent=vi.fn()
    const unsubscribe=subscribe('project/id',queryClient,onEvent)
    expect(FakeEventSource.latest.url).toBe('/api/v1/events/stream?projectId=project%2Fid')
    const event:EventRecord={id:42,projectId:'project/id',type:'task.retry_wait',aggregateType:'task',aggregateId:'task-id',resourceVersion:3,payload:{tail:'retrying'},occurredAt:'2026-07-13T00:00:00Z'}
    FakeEventSource.latest.emit(event)
    expect(onEvent).toHaveBeenCalledWith(event)
    expect(invalidateQueries).toHaveBeenCalledWith({queryKey:['projects']})
    expect(invalidateQueries).toHaveBeenCalledWith({queryKey:['plans','project/id']})
    expect(invalidateQueries).toHaveBeenCalledWith({queryKey:['events','project/id']})
    unsubscribe()
    expect(FakeEventSource.latest.close).toHaveBeenCalledOnce()
  })
})
