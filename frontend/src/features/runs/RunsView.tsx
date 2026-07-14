import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../../api/client'
import type { AgentRun, Project } from '../../api/types'
import { Activity } from '../../components/Icons'
import { Empty } from '../../components/Status'
import { parseTerminalLines } from './terminal'

const statusText:Record<AgentRun['status'],string>={starting:'正在启动',running:'运行中',succeeded:'已成功',failed:'失败',cancelled:'已取消',timed_out:'已超时'}
const providerText:Record<AgentRun['provider'],string>={codex:'Codex CLI',claude:'Claude CLI',validation:'最终验证'}

function formatDuration(ms:number){
  const seconds=Math.max(0,Math.floor(ms/1000))
  if(seconds<60)return `${seconds} 秒`
  const minutes=Math.floor(seconds/60)
  const rest=seconds%60
  if(minutes<60)return `${minutes} 分 ${rest} 秒`
  return `${Math.floor(minutes/60)} 小时 ${minutes%60} 分`
}
function formatTime(value:string){return new Intl.DateTimeFormat('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}).format(new Date(value))}

export function RunsView({project}:{project:Project}){
  const runs=useQuery({queryKey:['agent-runs',project.id],queryFn:()=>api.agentRuns(project.id),refetchInterval:1000})
  const[selected,setSelected]=useState<string>()
  const items=runs.data??[]
  useEffect(()=>{
    if(items.length===0){setSelected(undefined);return}
    if(!selected||!items.some(run=>run.id===selected))setSelected(items[0].id)
  },[items,selected])
  const run=items.find(item=>item.id===selected)
  const log=useQuery({queryKey:['agent-run-log',selected,'latest'],queryFn:()=>api.agentRunLog(selected!),enabled:!!selected,refetchInterval:1000})
  const [olderPages,setOlderPages]=useState<Awaited<ReturnType<typeof api.agentRunLog>>[]>([])
  const [loadingOlder,setLoadingOlder]=useState(false)
  const [olderError,setOlderError]=useState<string>()
  const logRef=useRef<HTMLDivElement>(null)
  const stickToBottom=useRef(true)
  const lastLogScrollTop=useRef(0)
  useEffect(()=>{setOlderPages([]);setOlderError(undefined);setLoadingOlder(false);stickToBottom.current=true;lastLogScrollTop.current=0},[selected])
  const displayedLines=useMemo(()=>[...olderPages.flatMap(page=>page.lines),...(log.data?.lines??[])],[olderPages,log.data?.lines])
  const logContent=useMemo(()=>displayedLines.join('\n'),[displayedLines])
  const terminalLines=useMemo(()=>run&&logContent?parseTerminalLines(logContent,run.provider):[],[logContent,run])
  const oldestPage=olderPages[0]??log.data
  const hasOlderLogs=oldestPage?.hasMore===true&&oldestPage.nextBefore!==undefined
  useEffect(()=>{const element=logRef.current;if(element&&stickToBottom.current)element.scrollTop=element.scrollHeight},[logContent,selected])
  const loadOlder=async()=>{
    if(!selected||loadingOlder||!hasOlderLogs||oldestPage?.nextBefore===undefined)return
    const element=logRef.current
    const previousHeight=element?.scrollHeight??0
    const previousTop=element?.scrollTop??0
    setLoadingOlder(true);setOlderError(undefined)
    try{
      const page=await api.agentRunLog(selected,oldestPage.nextBefore)
      setOlderPages(pages=>[page,...pages])
      requestAnimationFrame(()=>{if(element)element.scrollTop=element.scrollHeight-previousHeight+previousTop})
    }catch(error){setOlderError(error instanceof Error?error.message:'加载更早日志失败')}finally{setLoadingOlder(false)}
  }
  const onLogScroll=(event:React.UIEvent<HTMLDivElement>)=>{
    const element=event.currentTarget
    const movedUp=element.scrollTop<lastLogScrollTop.current-1
    const atBottom=element.scrollHeight-element.scrollTop-element.clientHeight<8
    if(movedUp)stickToBottom.current=false
    else if(atBottom)stickToBottom.current=true
    lastLogScrollTop.current=element.scrollTop
    if(element.scrollTop<72)void loadOlder()
  }
  const runningCount=useMemo(()=>items.filter(item=>item.status==='running'||item.status==='starting').length,[items])

  return <div className="page runs-page runs-scroll-page">
    <section className="hero compact"><div><span className="eyebrow">本地执行监控</span><h1>CLI 运行</h1><p>实时查看本地 Codex、Claude 和最终验证命令的输出。执行不设置超时时间，可通过停止自动化、计划或任务手动终止。</p></div><span className={`live-label ${runningCount?'running':''}`}><i/>{runningCount?`${runningCount} 个正在运行`:'实时监控已连接'}</span></section>
    <section className="runs-layout panel runs-scroll-layout">
      <aside className="run-list app-scroll-region">
        <header><strong>最近运行</strong><span>{items.length} 条</span></header>
        {runs.isLoading?<div className="run-list-message">正在加载运行记录…</div>:items.length===0?<Empty title="暂无 CLI 运行" body="生成计划、讨论需求或执行任务后，运行情况会显示在这里。"/>:items.map(item=>{
          const active=item.status==='running'||item.status==='starting'
          const duration=active?Date.now()-new Date(item.startedAt).getTime():item.durationMs
          return <button key={item.id} className={selected===item.id?'selected':''} onClick={()=>setSelected(item.id)}>
            <span className={`run-state ${item.status}`}><i/>{statusText[item.status]}</span>
            <strong>{providerText[item.provider]}</strong>
            <small title={item.commandSummary}>{item.commandSummary}</small>
            <span className="run-meta"><time>{formatTime(item.startedAt)}</time><b>{formatDuration(duration)}</b></span>
          </button>
        })}
      </aside>
      <div className="run-console runs-scroll-console">
        {!run?<Empty title="选择一条运行记录" body="运行日志会在 CLI 执行期间每秒自动刷新。"/>:<>
          <header>
            <div><span className={`run-state ${run.status}`}><i/>{statusText[run.status]}</span><h2>{providerText[run.provider]}</h2><p>{run.commandSummary}</p></div>
            <div className="run-header-side"><dl><div><dt>进程</dt><dd>{run.pid??'—'}</dd></div><div><dt>退出码</dt><dd>{run.exitCode??'—'}</dd></div><div><dt>日志大小</dt><dd>{log.data?`${Math.ceil(log.data.sizeBytes/1024)} KB`:'—'}</dd></div></dl></div>
          </header>
          <div ref={logRef} onScroll={onLogScroll} className="cli-log readable app-scroll-region">{log.isLoading?<div className="terminal-empty">正在读取日志…</div>:log.error?<div className="terminal-empty error">日志读取失败：{log.error.message}</div>:!logContent?<div className="terminal-empty">CLI 已启动，正在等待输出…</div>:<>{hasOlderLogs&&<div className="log-pagination">{loadingOlder?'正在加载更早的 50 条日志…':olderError?<button type="button" onClick={()=>void loadOlder()}>加载失败，点击重试</button>:'向上滚动可加载更早的 50 条日志'}</div>}{terminalLines.map((line,index)=><div key={`${index}-${line.text.slice(0,32)}`} className={`terminal-line ${line.kind}`}><span className="terminal-marker">{line.marker??'·'}</span><span>{line.text}</span></div>)}</>}</div>
          <footer><Activity/><span>{run.status==='running'||run.status==='starting'?'正在运行，无超时限制；默认展示最新 50 条简略日志。':'运行已结束；向上滚动可按 50 条继续查看更早的简略日志。'}</span></footer>
        </>}
      </div>
    </section>
  </div>
}
