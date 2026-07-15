import { useEffect, useMemo, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../../api/client'
import type {
  AgentRun,
  AgentRunProvider,
  ObservabilityAgentRun,
  ObservabilityRate,
  Plan,
  Project,
} from '../../api/types'
import { Activity } from '../../components/Icons'
import { Empty } from '../../components/Status'
import { buildUsageTrend, parseTerminalLines, runSessionBadges } from './terminal'

const statusText:Record<string,string>={starting:'正在启动',running:'运行中',cancelling:'正在取消',succeeded:'已成功',failed:'失败',interrupted:'已中断',cancelled:'已取消',timed_out:'已超时'}
const providerText:Record<AgentRunProvider,string>={codex:'Codex CLI',claude:'Claude CLI',validation:'最终验证'}
const operationText:Record<string,string>={requirement_discussion:'需求讨论',plan_generation:'计划生成',task_execution:'任务执行',validation:'最终验证'}
const failureText:Record<string,string>={non_zero_exit:'非零退出',provider_error:'Provider 错误',output_parse:'输出解析失败',validation:'验证失败',cancellation:'已取消',timeout:'超时',interrupted:'执行中断',unknown:'未知失败'}
const rangeOptions=[['24h','最近 24 小时'],['7d','最近 7 天'],['30d','最近 30 天'],['all','全部时间']] as const
type TimeRange=typeof rangeOptions[number][0]
type DetailedRun=AgentRun&{
  intakeId?:string
  planId?:string
  logicalOperationId?:string
  operationType?:ObservabilityAgentRun['operationType']
  jobAttempt?:number
  retryCount?:number
  sessionMode?:ObservabilityAgentRun['sessionMode']
  sessionInvalidationReason?:string
  queueWaitMs?:number
  failureCategory?:string
}
type ExportOptions={includeProjectName:boolean;includeWorkspacePath:boolean;includeBusinessTitles:boolean}

function formatDuration(ms:number){
  const seconds=Math.max(0,Math.floor(ms/1000))
  if(seconds<60)return `${seconds} 秒`
  const minutes=Math.floor(seconds/60)
  const rest=seconds%60
  if(minutes<60)return `${minutes} 分 ${rest} 秒`
  return `${Math.floor(minutes/60)} 小时 ${minutes%60} 分`
}
function formatTime(value:string){return new Intl.DateTimeFormat('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}).format(new Date(value))}
function formatDay(value:string){return new Intl.DateTimeFormat('zh-CN',{month:'2-digit',day:'2-digit'}).format(new Date(`${value}T00:00:00Z`))}
function formatNumber(value:number){return new Intl.NumberFormat('zh-CN',{maximumFractionDigits:1}).format(value)}
function formatCost(value:number|string,currency:string){
  const amount=typeof value==='number'?value:Number(value)
  return Number.isFinite(amount)?`${new Intl.NumberFormat('zh-CN',{maximumFractionDigits:6}).format(amount)} ${currency}`:`${value} ${currency}`
}
function rateValue(rate:ObservabilityRate|undefined){return rate?.available&&rate.value!==undefined?`${formatNumber(rate.value*100)}%`:'不可用'}
function rangeStart(range:TimeRange,anchor:Date){
  if(range==='all')return undefined
  const hours=range==='24h'?24:range==='7d'?24*7:24*30
  return new Date(anchor.getTime()-hours*60*60*1000).toISOString()
}
function relationTitle(run:ObservabilityAgentRun,requirements:Map<string,string>,plans:Map<string,string>,tasks:Map<string,string>){
  if(run.taskId)return tasks.get(run.taskId)||`任务 ${run.taskId.slice(0,8)}`
  if(run.planId)return plans.get(run.planId)||`计划 ${run.planId.slice(0,8)}`
  if(run.requirementId)return requirements.get(run.requirementId)||`需求 ${run.requirementId.slice(0,8)}`
  return operationText[run.operationType??'']||'CLI 调用'
}
function coverageText(covered:number,total:number){return `覆盖 ${covered}/${total} 次调用`}

function MetricCard({label,rate,loading}:{label:string;rate?:ObservabilityRate;loading:boolean}){
  return <article className={`run-metric-card ${!loading&&!rate?.available?'unavailable':''}`}>
    <span>{label}</span><strong>{loading?'加载中…':rateValue(rate)}</strong>
    <small>{loading?'正在计算数据覆盖':rate?.available?`${rate.numerator}/${rate.denominator} 次逻辑操作`:'当前范围没有可判定数据'}</small>
  </article>
}

export function RunsView({project,plans}:{project:Project;plans:Plan[]}){
  const[range,setRange]=useState<TimeRange>('7d')
  const[rangeAnchor,setRangeAnchor]=useState(()=>new Date())
  const[provider,setProvider]=useState<''|AgentRunProvider>('')
  const[planId,setPlanId]=useState('')
  const[selected,setSelected]=useState<string>()
  const[exportOptions,setExportOptions]=useState<ExportOptions>({includeProjectName:false,includeWorkspacePath:false,includeBusinessTitles:false})
  const[exportConfirmed,setExportConfirmed]=useState(false)
  const[exporting,setExporting]=useState<'json'|'csv'>()
  const[exportError,setExportError]=useState<string>()
  const from=useMemo(()=>rangeStart(range,rangeAnchor),[range,rangeAnchor])
  const query=useMemo(()=>({from,provider:provider||undefined,planId:planId||undefined,page:1,pageSize:200}),[from,provider,planId])
  const observability=useQuery({queryKey:['agent-run-observability',project.id,query],queryFn:()=>api.observability(project.id,query),refetchInterval:1000})
  const legacyRuns=useQuery({queryKey:['agent-runs',project.id,'metadata'],queryFn:()=>api.agentRuns(project.id,200),refetchInterval:1000})
  const data=observability.data
  const items=data?.runs??[]
  const metadata=useMemo(()=>new Map((legacyRuns.data as DetailedRun[]|undefined)?.map(run=>[run.id,run])??[]),[legacyRuns.data])

  useEffect(()=>{if(planId&&!plans.some(plan=>plan.id===planId))setPlanId('')},[planId,plans])
  useEffect(()=>{
    if(items.length===0){setSelected(undefined);return}
    if(!selected||!items.some(run=>run.id===selected))setSelected(items[0].id)
  },[items,selected])

  const run=items.find(item=>item.id===selected)
  const runMetadata=run?metadata.get(run.id):undefined
  const log=useQuery({queryKey:['agent-run-log',selected,'latest-50'],queryFn:()=>api.agentRunLog(selected!,undefined,50),enabled:!!selected,refetchInterval:1000})
  const[olderPages,setOlderPages]=useState<Awaited<ReturnType<typeof api.agentRunLog>>[]>([])
  const[loadingOlder,setLoadingOlder]=useState(false)
  const[olderError,setOlderError]=useState<string>()
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
      const page=await api.agentRunLog(selected,oldestPage.nextBefore,50)
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

  const requirements=useMemo(()=>new Map(data?.requirements.map(item=>[item.id,item.title||`需求 ${item.id.slice(0,8)}`])??[]),[data?.requirements])
  const planTitles=useMemo(()=>new Map([...(plans.map(item=>[item.id,item.title] as const)),...(data?.plans.map(item=>[item.id,item.title||`计划 ${item.id.slice(0,8)}`] as const)??[])]),[data?.plans,plans])
  const tasks=useMemo(()=>new Map(data?.tasks.map(item=>[item.id,item.title||item.taskKey])??[]),[data?.tasks])
  const usageTrend=useMemo(()=>buildUsageTrend(items),[items])
  const currencies=useMemo(()=>[...new Set(usageTrend.flatMap(point=>point.costs.map(cost=>cost.currency)))].sort(),[usageTrend])
  const durationMax=useMemo(()=>Math.max(1,...(data?.aggregates.durationTrend.flatMap(point=>[point.queueWait.averageMs??0,point.runDuration.averageMs??0])??[0])),[data?.aggregates.durationTrend])
  const tokenMax=useMemo(()=>Math.max(1,...usageTrend.map(point=>point.totalTokens??0)),[usageTrend])
  const runningCount=useMemo(()=>items.filter(item=>item.status==='running'||item.status==='starting').length,[items])
  const retries=useMemo(()=>{
    const retried=items.filter(item=>(item.retryCount??0)>0||(item.jobAttempt??1)>1)
    return {calls:retried.length,operations:new Set(retried.map(item=>item.logicalOperationId??item.id)).size,max:Math.max(0,...retried.map(item=>item.retryCount??Math.max(0,(item.jobAttempt??1)-1)))}
  },[items])
  const operationRuns=useMemo(()=>run?(run.logicalOperationId?items.filter(item=>item.logicalOperationId===run.logicalOperationId):[run]).slice().sort((a,b)=>a.startedAt.localeCompare(b.startedAt)):[],[items,run])
  const actualRange=items.length?`${formatTime(items[items.length-1].startedAt)} — ${formatTime(items[0].startedAt)}`:'暂无调用'
  const filterRangeLabel=rangeOptions.find(option=>option[0]===range)?.[1]??'全部时间'
  const sensitiveSelected=Object.values(exportOptions).some(Boolean)

  const chooseRange=(value:TimeRange)=>{setRangeAnchor(new Date());setRange(value)}
  const toggleExportOption=(key:keyof ExportOptions)=>{setExportOptions(current=>({...current,[key]:!current[key]}));setExportConfirmed(false);setExportError(undefined)}
  const exportData=async(format:'json'|'csv')=>{
    if(sensitiveSelected&&!exportConfirmed)return
    setExporting(format);setExportError(undefined)
    try{
      const result=await api.exportObservability(project.id,{from,provider:provider||undefined,planId:planId||undefined,format,...exportOptions})
      const url=URL.createObjectURL(result.blob)
      const link=document.createElement('a');link.href=url;link.download=result.filename;document.body.appendChild(link);link.click();link.remove();URL.revokeObjectURL(url)
    }catch(error){setExportError(error instanceof Error?error.message:'导出失败')}finally{setExporting(undefined)}
  }

  return <div className="page runs-page runs-scroll-page app-scroll-region">
    <section className="hero compact"><div><span className="eyebrow">本地执行监控</span><h1>CLI 运行</h1><p>项目由左侧边栏筛选；下方筛选会同步作用于指标、趋势、运行列表和导出范围。</p></div><span className={`live-label ${runningCount?'running':''}`}><i/>{runningCount?`${runningCount} 个筛选内运行中`:'实时监控已连接'}</span></section>

    <section className="run-filter-panel panel" aria-label="运行筛选器">
      <div className="run-filter-group"><span>时间范围</span><div className="run-filter-chips">{rangeOptions.map(([value,label])=><button type="button" key={value} className={range===value?'active':''} onClick={()=>chooseRange(value)}>{label}</button>)}</div></div>
      <label><span>Provider</span><select value={provider} onChange={event=>setProvider(event.target.value as ''|AgentRunProvider)}><option value="">全部 Provider</option><option value="codex">Codex CLI</option><option value="claude">Claude CLI</option><option value="validation">最终验证</option></select></label>
      <label><span>计划</span><select value={planId} onChange={event=>setPlanId(event.target.value)}><option value="">全部计划</option>{plans.map(plan=><option key={plan.id} value={plan.id}>{plan.title}</option>)}</select></label>
      <div className="run-filter-coverage"><span>数据覆盖范围</span><strong>{filterRangeLabel} · {actualRange}</strong><small>{data?`指标 ${data.pagination.totalItems} 次调用；列表 ${items.length}/${data.pagination.totalItems}`:'正在读取结构化运行摘要'}</small></div>
    </section>

    {observability.isError&&<section className="run-request-state error" role="alert"><div><strong>运行指标请求失败</strong><span>{observability.error instanceof Error?observability.error.message:'请稍后重试。'}</span></div><button className="button small secondary" onClick={()=>void observability.refetch()}>重试</button></section>}

    <section className="run-metric-grid" aria-label="质量指标">
      <MetricCard label="会话复用率" rate={data?.aggregates.sessionReuseRate} loading={observability.isLoading}/>
      <MetricCard label="快照恢复率" rate={data?.aggregates.snapshotRestoreRate} loading={observability.isLoading}/>
      <MetricCard label="计划生成成功率" rate={data?.aggregates.planGenerationSuccessRate} loading={observability.isLoading}/>
      <MetricCard label="任务执行成功率" rate={data?.aggregates.taskExecutionSuccessRate} loading={observability.isLoading}/>
    </section>

    <section className="run-insights-grid">
      <article className="panel run-trend-panel"><header><div><span className="eyebrow">耗时趋势</span><h2>等待与运行耗时</h2></div><small>{data?`${data.aggregates.durationTrend.length} 个时间桶`:'加载中'}</small></header>
        {observability.isLoading?<div className="run-panel-state">正在加载耗时趋势…</div>:!data?.aggregates.durationTrend.length?<div className="run-panel-state">暂无耗时数据</div>:<div className="duration-trend app-scroll-region">{data.aggregates.durationTrend.map(point=><div className="duration-point" key={point.bucket}><span>{formatDay(point.bucket)}</span><div><i className="queue" style={{width:`${((point.queueWait.averageMs??0)/durationMax)*100}%`}}/><i className="runtime" style={{width:`${((point.runDuration.averageMs??0)/durationMax)*100}%`}}/></div><small>等待 {point.queueWait.available&&point.queueWait.averageMs!==undefined?formatDuration(point.queueWait.averageMs):'不可用'} · 运行 {point.runDuration.available&&point.runDuration.averageMs!==undefined?formatDuration(point.runDuration.averageMs):'不可用'}</small></div>)}</div>}
        <footer><span><i className="queue"/>平均等待</span><span><i className="runtime"/>平均运行</span></footer>
      </article>
      <article className="panel run-quality-panel"><header><div><span className="eyebrow">质量诊断</span><h2>失败类别与重试</h2></div></header>
        {observability.isLoading?<div className="run-panel-state">正在加载质量数据…</div>:<><div className="retry-summary"><div><strong>{retries.operations}</strong><span>发生重试的逻辑操作</span></div><div><strong>{retries.calls}</strong><span>重试调用</span></div><div><strong>{retries.max}</strong><span>最高重试次数</span></div></div>{data?.aggregates.failureCategories.length?<div className="failure-list">{data.aggregates.failureCategories.map(item=><div key={item.category}><span>{failureText[item.category]??item.category}</span><strong>{item.count}</strong></div>)}</div>:<div className="run-panel-state compact">当前范围没有失败类别</div>}</>}
      </article>
    </section>

    <section className="panel usage-panel"><header><div><span className="eyebrow">用量趋势</span><h2>Token 与费用</h2><p>仅汇总 Provider 实际返回的数据；缺失值显示“不可用”，费用按币种分别展示，不做换算。</p></div><div className="usage-summary"><div><span>Token 总量</span><strong>{data?.aggregates.usage.overall.tokens.available&&data.aggregates.usage.overall.tokens.totalTokens!==undefined?formatNumber(data.aggregates.usage.overall.tokens.totalTokens):'不可用'}</strong><small>{data?coverageText(data.aggregates.usage.overall.tokens.coverageCount,data.aggregates.usage.overall.tokens.totalRunCount):'加载中'}</small></div><div><span>费用</span>{data?.aggregates.usage.overall.costs.available?data.aggregates.usage.overall.costs.currencies.map(cost=><strong key={cost.currency}>{formatCost(cost.amount,cost.currency)}</strong>):<strong>不可用</strong>}<small>{data?coverageText(data.aggregates.usage.overall.costs.coverageCount,data.aggregates.usage.overall.costs.totalRunCount):'加载中'}</small></div></div></header>
      <div className="usage-trends">
        <div className="usage-trend"><h3>Token 趋势</h3>{observability.isLoading?<div className="run-panel-state">正在加载…</div>:!usageTrend.some(point=>point.totalTokens!==undefined)?<div className="run-unavailable">不可用<small>筛选范围内没有已上报的 Token 数据</small></div>:<div className="usage-bars app-scroll-region">{usageTrend.map(point=><div key={point.bucket}><span>{formatDay(point.bucket)}</span><i><b style={{height:`${((point.totalTokens??0)/tokenMax)*100}%`}}/></i><strong>{point.totalTokens===undefined?'不可用':formatNumber(point.totalTokens)}</strong></div>)}</div>}</div>
        <div className="usage-trend"><h3>费用趋势</h3>{observability.isLoading?<div className="run-panel-state">正在加载…</div>:currencies.length===0?<div className="run-unavailable">不可用<small>筛选范围内没有已上报的费用数据</small></div>:<div className="currency-trends app-scroll-region">{currencies.map(currency=>{const max=Math.max(1,...usageTrend.map(point=>point.costs.find(cost=>cost.currency===currency)?.amount??0));return <section key={currency}><header><strong>{currency}</strong><span>独立币种，不换算</span></header>{usageTrend.map(point=>{const cost=point.costs.find(item=>item.currency===currency);return <div key={point.bucket}><span>{formatDay(point.bucket)}</span><i><b style={{width:`${((cost?.amount??0)/max)*100}%`}}/></i><strong>{cost?formatCost(cost.amount,currency):'不可用'}</strong></div>})}</section>})}</div>}</div>
      </div>
      {data&&data.pagination.hasMore&&<div className="coverage-warning">Token 与费用趋势覆盖当前列表的最近 {items.length} 次调用；聚合总量覆盖全部 {data.pagination.totalItems} 次调用。</div>}
    </section>

    <details className="panel run-export-panel"><summary><div><span className="eyebrow">结构化摘要</span><strong>导出 JSON / CSV</strong></div><span>设置敏感元数据</span></summary><div className="run-export-body"><p>导出仅包含结构化摘要，不包含 CLI 会话 ID、命令参数、环境变量、原始错误正文或日志内容。导出范围与当前项目及筛选条件完全一致。</p><fieldset><legend>敏感元数据（默认关闭）</legend><label><input type="checkbox" checked={exportOptions.includeProjectName} onChange={()=>toggleExportOption('includeProjectName')}/>项目名称</label><label><input type="checkbox" checked={exportOptions.includeWorkspacePath} onChange={()=>toggleExportOption('includeWorkspacePath')}/>工作区路径</label><label><input type="checkbox" checked={exportOptions.includeBusinessTitles} onChange={()=>toggleExportOption('includeBusinessTitles')}/>需求、计划和任务业务标题</label></fieldset>{sensitiveSelected&&<label className="export-confirm"><input type="checkbox" checked={exportConfirmed} onChange={event=>setExportConfirmed(event.target.checked)}/>我确认所选字段可能包含敏感元数据，并同意将其写入导出文件。</label>}<div className="run-export-actions"><button className="button secondary" disabled={!!exporting||(sensitiveSelected&&!exportConfirmed)} onClick={()=>void exportData('json')}>{exporting==='json'?'正在导出…':'导出 JSON'}</button><button className="button secondary" disabled={!!exporting||(sensitiveSelected&&!exportConfirmed)} onClick={()=>void exportData('csv')}>{exporting==='csv'?'正在导出…':'导出 CSV'}</button><span>{project.name} · {filterRangeLabel} · {provider?providerText[provider]:'全部 Provider'} · {planId?planTitles.get(planId):'全部计划'}</span></div>{exportError&&<div className="run-export-error" role="alert">{exportError}</div>}</div></details>

    <section className="runs-layout panel runs-scroll-layout">
      <aside className="run-list app-scroll-region">
        <header><strong>筛选内运行</strong><span>{data?`${items.length}/${data.pagination.totalItems}`:'—'}</span></header>
        {observability.isLoading?<div className="run-list-message">正在加载运行记录…</div>:observability.isError?<div className="run-list-message error">运行列表请求失败</div>:items.length===0?<Empty title="当前筛选无 CLI 运行" body="切换时间范围、Provider 或计划后再试。"/>:items.map(item=>{
          const detail=metadata.get(item.id)
          const badges=runSessionBadges({...item,sessionInvalidationReason:detail?.sessionInvalidationReason})
          return <button key={item.id} className={selected===item.id?'selected':''} onClick={()=>setSelected(item.id)}><div className={`run-state ${item.status}`}><i/>{statusText[item.status]??item.status}</div><strong>{relationTitle(item,requirements,planTitles,tasks)}</strong><small>{operationText[item.operationType??'']??providerText[item.provider]} · {providerText[item.provider]}</small><div className="run-badges">{badges.map(badge=><span key={badge.key} className={badge.tone}>{badge.label}</span>)}{(item.retryCount??0)>0&&<span className="retry">重试 {item.retryCount} 次</span>}{item.failureCategory&&<span className="failure">{failureText[item.failureCategory]??item.failureCategory}</span>}</div><div className="run-meta"><span>{formatTime(item.startedAt)}</span><b>{item.durationMs===undefined?'耗时不可用':formatDuration(item.durationMs)}</b></div></button>
        })}
      </aside>
      {run?<article className="run-console runs-scroll-console">
        <header><div><div className={`run-state ${run.status}`}><i/>{statusText[run.status]??run.status}</div><h2>{relationTitle(run,requirements,planTitles,tasks)}</h2><p>{runMetadata?.commandSummary||`${operationText[run.operationType??'']??'CLI 调用'} · 结构化运行摘要`}</p><div className="run-badges detail">{runSessionBadges({...run,sessionInvalidationReason:runMetadata?.sessionInvalidationReason}).map(badge=><span key={badge.key} className={badge.tone}>{badge.label}</span>)}{run.failureCategory&&<span className="failure">失败：{failureText[run.failureCategory]??run.failureCategory}</span>}{run.outputTruncated&&<span className="warning">输出已截断</span>}</div></div><div className="run-header-side"><span className="provider-badge">{providerText[run.provider]}</span><dl><div><dt>排队等待</dt><dd>{run.queueWaitMs===undefined?'不可用':formatDuration(run.queueWaitMs)}</dd></div><div><dt>运行耗时</dt><dd>{run.durationMs===undefined?'不可用':formatDuration(run.durationMs)}</dd></div><div><dt>重试</dt><dd>{run.retryCount===undefined?'不可用':`${run.retryCount} 次`}</dd></div></dl></div></header>
        <section className="run-context"><div className="run-lineage"><span>关联路径</span><strong>{run.requirementId?requirements.get(run.requirementId)||`需求 ${run.requirementId.slice(0,8)}`:'无需求'} <b>›</b> {run.planId?planTitles.get(run.planId)||`计划 ${run.planId.slice(0,8)}`:'无计划'} <b>›</b> {run.taskId?tasks.get(run.taskId)||`任务 ${run.taskId.slice(0,8)}`:'无任务'}</strong></div><div className="run-detail-usage"><span>Token <strong>{run.totalTokens===undefined?'不可用':formatNumber(run.totalTokens)}</strong></span><span>费用 <strong>{run.costAmount===undefined||!run.costCurrency?'不可用':formatCost(run.costAmount,run.costCurrency)}</strong></span><span>输出 <strong>{run.outputLines===undefined?'不可用':`${formatNumber(run.outputLines)} 行`}</strong></span></div><div className="operation-chain"><header><span>同一逻辑操作的物理调用</span><strong>{operationRuns.length} 次</strong></header><div>{operationRuns.map((item,index)=><button key={item.id} className={item.id===run.id?'active':''} onClick={()=>setSelected(item.id)}><span>#{index+1}</span><b>{providerText[item.provider]}</b><small>{statusText[item.status]??item.status}</small>{runSessionBadges({...item,sessionInvalidationReason:metadata.get(item.id)?.sessionInvalidationReason}).map(badge=><i key={badge.key} className={badge.tone}>{badge.label}</i>)}</button>)}</div></div></section>
        {run.provider!=='codex'&&<div className="log-notice">为避免渲染大型原始日志，非 Codex 输出仅显示概括信息。</div>}
        <div ref={logRef} className="cli-log readable app-scroll-region" onScroll={onLogScroll}>{(hasOlderLogs||loadingOlder||olderError)&&<div className="log-pagination">{loadingOlder?'正在加载更早 50 条日志…':olderError?<><span>{olderError}</span> <button onClick={()=>void loadOlder()}>重试</button></>:<button onClick={()=>void loadOlder()}>向上滚动或点击加载更早 50 条</button>}</div>}{log.isLoading?<div className="terminal-empty">正在连接实时日志…</div>:log.isError?<div className="terminal-empty error">日志读取失败：{log.error instanceof Error?log.error.message:'请稍后重试'}</div>:terminalLines.length?terminalLines.map((line,index)=><div className={`terminal-line ${line.kind}`} key={`${index}-${line.marker}-${line.text}`}><span className="terminal-marker">{line.marker??'│'}</span><span>{line.text}</span></div>):<div className="terminal-empty">运行已记录，暂时还没有终端输出。</div>}</div>
        <footer><Activity/><span>{run.status==='running'||run.status==='starting'?'每秒刷新 · 默认最新 50 条 · 向上滚动每次加载更早 50 条':'运行记录已结束 · 默认最新 50 条 · 可向上加载更早内容'}</span></footer>
      </article>:<div className="run-console"><Empty title="选择一条运行" body="可查看结构化详情、同一逻辑操作的多次调用和实时终端输出。"/></div>}
    </section>
  </div>
}
