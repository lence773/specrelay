import type { QueryClient } from '@tanstack/react-query'
import type { EventRecord } from './types'

export function subscribe(projectId:string,queryClient:QueryClient,onEvent:(event:EventRecord)=>void){
  const stream=new EventSource(`/api/v1/events/stream?projectId=${encodeURIComponent(projectId)}`)
  stream.onmessage=message=>{
    const event=JSON.parse(message.data) as EventRecord
    onEvent(event)
    queryClient.invalidateQueries({queryKey:['projects']})
    if(event.aggregateType==='intake')queryClient.invalidateQueries({queryKey:['intakes',projectId]})
    if(event.aggregateType==='plan'||event.aggregateType==='task')queryClient.invalidateQueries({queryKey:['plans',projectId]})
    if(event.aggregateType==='agent_run'){
      queryClient.invalidateQueries({queryKey:['agent-runs',projectId]})
      queryClient.invalidateQueries({queryKey:['agent-run-log',event.aggregateId]})
    }
    queryClient.invalidateQueries({queryKey:['events',projectId]})
  }
  return()=>stream.close()
}
