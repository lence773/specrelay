import { useEffect, useMemo, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../api/client'
import { subscribe } from '../../api/events'
import type { EventRecord } from '../../api/types'
import { Activity } from '../../components/Icons'
import { Empty, eventLabel, relative, resourceLabel } from '../../components/Status'

const PAGE_SIZE=10
const LIVE_BUFFER_LIMIT=500

type EventPageState={
  items:EventRecord[]
  hasOlder:boolean
  nextBefore:number|null
}

type EventsViewProps={
  events:EventRecord[]
  projectId?:string
}

function visibleEvents(events:EventRecord[]){
  const unique=new Map<number,EventRecord>()
  for(const event of events){
    if(event.type==='agent.output'||unique.has(event.id))continue
    unique.set(event.id,event)
  }
  return [...unique.values()].sort((left,right)=>right.id-left.id)
}

function latestPage(events:EventRecord[],knownHasOlder=events.length>PAGE_SIZE):EventPageState{
  const items=visibleEvents(events).slice(0,PAGE_SIZE)
  const hasOlder=knownHasOlder&&items.length>0
  return {items,hasOlder,nextBefore:hasOlder?items.at(-1)!.id:null}
}

export function EventsView({events,projectId}:EventsViewProps) {
  const queryClient=useQueryClient()
  const inputEvents=useMemo(()=>visibleEvents(events),[events])
  const activeProjectId=projectId??inputEvents.find(event=>event.projectId)?.projectId??undefined
  const[page,setPage]=useState<EventPageState>(()=>latestPage(inputEvents))
  const[newerPages,setNewerPages]=useState<EventPageState[]>([])
  const[hasNewEvents,setHasNewEvents]=useState(false)
  const[isLoading,setIsLoading]=useState(false)
  const[error,setError]=useState('')
  const pageRef=useRef(page)
  const pageDepthRef=useRef(0)
  const projectRef=useRef(activeProjectId)
  const knownEventIdsRef=useRef(new Set(inputEvents.map(event=>event.id)))
  const liveEventsRef=useRef<EventRecord[]>([])

  function replacePage(next:EventPageState){
    pageRef.current=next
    setPage(next)
  }

  function replaceNewerPages(next:EventPageState[]){
    pageDepthRef.current=next.length
    setNewerPages(next)
  }

  useEffect(()=>{
    if(projectRef.current!==activeProjectId){
      projectRef.current=activeProjectId
      knownEventIdsRef.current=new Set(inputEvents.map(event=>event.id))
      liveEventsRef.current=[]
      replacePage(latestPage(inputEvents))
      replaceNewerPages([])
      setHasNewEvents(false)
      setError('')
      return
    }

    const unseen=inputEvents.filter(event=>!knownEventIdsRef.current.has(event.id))
    for(const event of unseen)knownEventIdsRef.current.add(event.id)
    if(unseen.length>0){
      liveEventsRef.current=visibleEvents([...unseen,...liveEventsRef.current]).slice(0,LIVE_BUFFER_LIMIT)
      if(pageDepthRef.current>0){
        setHasNewEvents(true)
        return
      }
    }
    if(pageDepthRef.current===0){
      const merged=visibleEvents([...inputEvents,...liveEventsRef.current,...pageRef.current.items])
      const hasOlder=pageRef.current.hasOlder||merged.length>PAGE_SIZE||inputEvents.length>PAGE_SIZE
      replacePage(latestPage(merged,hasOlder))
    }
  },[activeProjectId,inputEvents])

  useEffect(()=>{
    if(!activeProjectId)return
    const after=inputEvents[0]?.id??0
    return subscribe(activeProjectId,after,queryClient,event=>{
      if(event.type==='agent.output'||knownEventIdsRef.current.has(event.id))return
      knownEventIdsRef.current.add(event.id)
      liveEventsRef.current=visibleEvents([event,...liveEventsRef.current]).slice(0,LIVE_BUFFER_LIMIT)
      if(pageDepthRef.current>0){
        setHasNewEvents(true)
        return
      }
      setPage(current=>{
        const merged=visibleEvents([event,...current.items])
        const next=latestPage(merged,current.hasOlder||merged.length>PAGE_SIZE)
        pageRef.current=next
        return next
      })
    })
  },[activeProjectId,queryClient])

  async function loadOlder(){
    const before=pageRef.current.nextBefore
    if(!activeProjectId||before===null||isLoading)return
    setIsLoading(true)
    setError('')
    try{
      const result=await api.events(activeProjectId,before,PAGE_SIZE)
      const items=visibleEvents(result.items).slice(0,PAGE_SIZE)
      for(const event of items)knownEventIdsRef.current.add(event.id)
      if(items.length===0){
        replacePage({...pageRef.current,hasOlder:false,nextBefore:null})
        return
      }
      const current=pageRef.current
      replaceNewerPages([...newerPages,current])
      replacePage({
        items,
        hasOlder:result.hasMore&&result.nextBefore!==null,
        nextBefore:result.hasMore?result.nextBefore:null,
      })
    }catch(cause){
      setError(cause instanceof Error?cause.message:'无法加载更早的事件。')
    }finally{
      setIsLoading(false)
    }
  }

  async function returnLatest(){
    if(!activeProjectId||isLoading)return
    setIsLoading(true)
    setError('')
    try{
      const result=await api.events(activeProjectId,undefined,PAGE_SIZE)
      const serverEvents=visibleEvents(result.items)
      for(const event of serverEvents)knownEventIdsRef.current.add(event.id)
      const merged=visibleEvents([...liveEventsRef.current,...serverEvents])
      replacePage(latestPage(merged,result.hasMore||merged.length>PAGE_SIZE))
      replaceNewerPages([])
      setHasNewEvents(false)
    }catch(cause){
      setError(cause instanceof Error?cause.message:'无法同步最新事件。')
    }finally{
      setIsLoading(false)
    }
  }

  function loadNewer(){
    if(newerPages.length===0||isLoading)return
    if(newerPages.length===1){
      void returnLatest()
      return
    }
    const nextPages=newerPages.slice(0,-1)
    replacePage(newerPages.at(-1)!)
    replaceNewerPages(nextPages)
  }

  const pageNumber=newerPages.length+1

  return <div className="page narrow">
    <section className="hero compact"><div><span className="eyebrow">不可变历史记录</span><h1>事件流</h1><p>实时查看保存在 PostgreSQL 中的资源变更、队列状态和智能体活动。</p></div><span className="live-label"><i/> SSE 已连接</span></section>
    <section className="panel event-panel">
      <header className="event-toolbar">
        <div className="event-page-status" aria-live="polite"><strong>第 {pageNumber} 页</strong><span>{pageNumber===1?'最新事件':'历史事件'}</span></div>
        <div className="event-pagination">
          <button type="button" disabled={pageNumber===1||isLoading} onClick={loadNewer}>上一页</button>
          <button type="button" disabled={!page.hasOlder||isLoading} onClick={()=>void loadOlder()}>下一页</button>
          {pageNumber>1&&<button type="button" className="latest-button" disabled={isLoading} onClick={()=>void returnLatest()}>返回最新页</button>}
        </div>
      </header>
      {hasNewEvents&&pageNumber>1&&<div className="event-new-notice" role="status"><span>有新事件到达，当前历史页保持不变。</span><button type="button" disabled={isLoading} onClick={()=>void returnLatest()}>返回最新页</button></div>}
      {error&&<div className="event-page-error" role="alert">{error}</div>}
      {page.items.length===0?<Empty title="正在等待活动" body="新事件会实时显示，并在重新连接后继续保留。"/>:<div className="event-table"><div className="event-head"><span>事件</span><span>资源</span><span>版本</span><span>时间</span></div>{page.items.map(event=><div className="event-row" data-event-id={event.id} key={event.id}><span><i className="event-icon"><Activity/></i><strong>{eventLabel(event.type)}</strong></span><span><b>{resourceLabel(event.aggregateType)}</b><code>{event.aggregateId.slice(0,8)}</code></span><span>v{event.resourceVersion}</span><span>{relative(event.occurredAt)}</span></div>)}</div>}
    </section>
  </div>
}
