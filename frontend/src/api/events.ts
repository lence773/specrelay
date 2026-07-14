import type { QueryClient } from '@tanstack/react-query'
import type { EventRecord } from './types'

export function subscribe(projectId:string,after:number,queryClient:QueryClient,onEvent:(event:EventRecord)=>void){
  const query=new URLSearchParams({projectId,after:String(after)})
  const stream=new EventSource(`/api/v1/events/stream?${query}`)
  const seenEventIds=new Set<number>(after>0?[after]:[])
  stream.onmessage=message=>{
    const event=JSON.parse(message.data) as EventRecord
    if(event.type==='agent.output'||seenEventIds.has(event.id))return
    seenEventIds.add(event.id)
    onEvent(event)
    if(event.aggregateType==='project')queryClient.invalidateQueries({queryKey:['projects']})
    if(event.aggregateType==='intake')queryClient.invalidateQueries({queryKey:['intakes',projectId]})
    if(event.aggregateType==='plan'||event.aggregateType==='task')queryClient.invalidateQueries({queryKey:['plans',projectId]})
    if(event.aggregateType==='agent_run'){
      queryClient.invalidateQueries({queryKey:['agent-runs',projectId]})
      queryClient.invalidateQueries({queryKey:['agent-run-log',event.aggregateId]})
    }
  }
  return()=>stream.close()
}
