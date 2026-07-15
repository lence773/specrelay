import type { AgentRun, ObservabilityAgentRun } from '../../api/types'

export type TerminalLineKind='system'|'command'|'output'|'working'|'success'|'error'|'todo'|'assistant'|'notice'

export interface TerminalLine {
  kind:TerminalLineKind
  text:string
  marker?:string
}

export interface RunBadge {
  key:string
  label:string
  tone:'new'|'reused'|'restored'|'warning'|'neutral'
}

export interface UsageTrendCost {
  currency:string
  amount:number
  coverageCount:number
}

export interface UsageTrendPoint {
  bucket:string
  runCount:number
  tokenCoverageCount:number
  totalTokens?:number
  costs:UsageTrendCost[]
}

const sessionModeText:Record<NonNullable<ObservabilityAgentRun['sessionMode']>,Omit<RunBadge,'key'>>={
  new:{label:'新会话',tone:'new'},
  reused:{label:'复用会话',tone:'reused'},
  snapshot_restored:{label:'快照恢复',tone:'restored'},
  not_applicable:{label:'无需会话',tone:'neutral'},
}
const invalidationText:Record<string,string>={provider_switched:'Provider 已切换',session_not_found:'会话不存在',restore_failed:'快照恢复失败'}

/** Produces explicit, redaction-safe session labels for run list and detail views. */
export function runSessionBadges(run:Pick<ObservabilityAgentRun,'sessionMode'>&{sessionInvalidationReason?:string}):RunBadge[]{
  const badges:RunBadge[]=[]
  if(run.sessionMode){const badge=sessionModeText[run.sessionMode];badges.push({key:`session-${run.sessionMode}`,...badge})}
  if(run.sessionInvalidationReason){
    const reason=invalidationText[run.sessionInvalidationReason]??run.sessionInvalidationReason.replaceAll('_',' ')
    badges.push({key:`invalidation-${run.sessionInvalidationReason}`,label:`会话失效：${reason}`,tone:'warning'})
  }
  return badges
}

/** Groups only reported usage values. Missing token/cost data stays undefined and is never converted to zero. */
export function buildUsageTrend(runs:ObservabilityAgentRun[]):UsageTrendPoint[]{
  const buckets=new Map<string,{runCount:number;tokenCoverageCount:number;totalTokens:number;costs:Map<string,{amount:number;coverageCount:number}>}>()
  for(const run of runs){
    const bucket=run.startedAt.slice(0,10)
    const point=buckets.get(bucket)??{runCount:0,tokenCoverageCount:0,totalTokens:0,costs:new Map()}
    point.runCount++
    if(run.totalTokens!==undefined){point.tokenCoverageCount++;point.totalTokens+=run.totalTokens}
    if(run.costAmount!==undefined&&run.costCurrency){
      const amount=Number(run.costAmount)
      if(Number.isFinite(amount)){
        const current=point.costs.get(run.costCurrency)??{amount:0,coverageCount:0}
        current.amount+=amount;current.coverageCount++
        point.costs.set(run.costCurrency,current)
      }
    }
    buckets.set(bucket,point)
  }
  return [...buckets.entries()].sort(([a],[b])=>a.localeCompare(b)).map(([bucket,point])=>({
    bucket,
    runCount:point.runCount,
    tokenCoverageCount:point.tokenCoverageCount,
    ...(point.tokenCoverageCount?{totalTokens:point.totalTokens}:{}),
    costs:[...point.costs.entries()].sort(([a],[b])=>a.localeCompare(b)).map(([currency,cost])=>({currency,...cost})),
  }))
}

type JSONRecord=Record<string,unknown>

const MAX_SUMMARY_CHARS=500

function isRecord(value:unknown):value is JSONRecord{return !!value&&typeof value==='object'&&!Array.isArray(value)}
function asString(value:unknown){return typeof value==='string'?value:''}
function asNumber(value:unknown){return typeof value==='number'&&Number.isFinite(value)?value:undefined}
function stripAnsi(value:string){return value.replace(/\u001B\[[0-?]*[ -/]*[@-~]/g,'')}
function trimCommand(command:string){const trimmed=command.replace(/^\/bin\/bash -lc ['"]?/, '').replace(/['"]?$/, '').replace(/\s+/g,' ').trim();return trimmed.length>220?`${trimmed.slice(0,217)}…`:trimmed}
function compactNumber(value:number){return new Intl.NumberFormat('zh-CN',{notation:'compact',maximumFractionDigits:1}).format(value)}

function compactText(value:string,maxChars=MAX_SUMMARY_CHARS){
  const compact=stripAnsi(value).replace(/\s+/g,' ').trim()
  return compact.length>maxChars?`${compact.slice(0,maxChars-1)}…`:compact
}

function todoLines(value:unknown):TerminalLine[]{
  if(!Array.isArray(value))return []
  return value.flatMap(todo=>{
    if(!isRecord(todo))return []
    const text=asString(todo.text)
    if(!text)return []
    const completed=todo.completed===true
    return [{kind:'todo',marker:completed?'✓':'○',text}]
  })
}


function planSummary(text:string):TerminalLine[]|undefined{
  const trimmed=text.trim()
  if(!trimmed.startsWith('{'))return undefined
  try{
    const data:unknown=JSON.parse(trimmed)
    if(!isRecord(data)||typeof data.title!=='string'||!Array.isArray(data.tasks))return undefined
    const lines:TerminalLine[]=[{kind:'assistant',marker:'✦',text:`计划已生成：${data.title}`}]
    const summary=asString(data.summary)
    if(summary)lines.push({kind:'assistant',marker:' ',text:compactText(summary)})
    lines.push({kind:'success',marker:'✓',text:`已整理 ${data.tasks.length} 个实施项。`})
    return lines
  }catch{return undefined}
}

function messageLines(value:string):TerminalLine[]{
  const summary=planSummary(value)
  if(summary)return summary
  return compactText(value)?[{kind:'assistant',marker:'✦',text:'CLI 已返回结果（为保持运行记录简洁，未展示正文）。'}]:[]
}

function commandLines(item:JSONRecord,completed:boolean):TerminalLine[]{
  const command=trimCommand(asString(item.command))
  const lines:TerminalLine[]=command?[{kind:'command',marker:'$',text:command}]:[]
  if(completed){
    const exitCode=asNumber(item.exit_code)
    const omitted=asString(item.aggregated_output)?'，已省略具体输出':' '
    if(exitCode===undefined)lines.push({kind:'success',marker:'✓',text:`命令执行完成${omitted}。`})
    else if(exitCode===0)lines.push({kind:'success',marker:'✓',text:`命令执行完成（退出码 0${omitted}）。`})
    else lines.push({kind:'error',marker:'×',text:`命令执行失败（退出码 ${exitCode}${omitted}）。`})
  }
  return lines
}

function toolName(item:JSONRecord){
  return asString(item.server)||asString(item.tool_name)||asString(item.name)||asString(item.type)||'工具'
}

function codexEventLines(event:JSONRecord):TerminalLine[]{
  const type=asString(event.type)
  if(type==='thread.started'){
    const id=asString(event.thread_id)
    return [{kind:'system',marker:'›',text:id?`已连接 Codex CLI（会话 ${id.slice(0,8)}…）`:'已连接 Codex CLI。'}]
  }
  if(type==='turn.started')return [{kind:'system',marker:'›',text:'开始处理请求。'}]
  if(type==='turn.completed'){
    const usage=isRecord(event.usage)?event.usage:undefined
    const input=usage?asNumber(usage.input_tokens):undefined
    const output=usage?asNumber(usage.output_tokens):undefined
    const details=[input!==undefined?`输入 ${compactNumber(input)} tokens`:'',output!==undefined?`输出 ${compactNumber(output)} tokens`:''].filter(Boolean).join(' · ')
    return [{kind:'success',marker:'✓',text:details?`本轮处理完成（${details}）。`:'本轮处理完成。'}]
  }
  if(type==='error'){
    const message=asString(event.message)||asString(event.error)||'Codex CLI 返回了错误。'
    return [{kind:'error',marker:'×',text:message}]
  }
  if(!type.startsWith('item.'))return []
  const item=isRecord(event.item)?event.item:undefined
  if(!item)return []
  const itemType=asString(item.type)
  const completed=type==='item.completed'
  if(itemType==='todo_list')return todoLines(item.items)
  if(itemType==='command_execution')return commandLines(item,completed)
  if(itemType==='agent_message')return completed?messageLines(asString(item.text)): [{kind:'working',marker:'…',text:'正在整理回复…'}]
  if(itemType==='reasoning')return completed?[]:[{kind:'working',marker:'…',text:'正在分析需求与项目代码…'}]
  if(itemType==='file_change'){
    const files=Array.isArray(item.changes)?item.changes.length:undefined
    return [{kind:completed?'success':'working',marker:completed?'✓':'…',text:files===undefined?`${completed?'已完成':'正在进行'}文件修改。`:`${completed?'已完成':'正在进行'} ${files} 个文件的修改。`}]
  }
  if(itemType==='mcp_tool_call'||itemType==='web_search')return [{kind:completed?'success':'working',marker:completed?'✓':'…',text:`${completed?'已完成':'正在执行'}工具调用：${toolName(item)}。`}]
  return completed?[{kind:'notice',marker:'›',text:`已完成 CLI 步骤：${itemType||'未知步骤'}。`}]:[]
}

/** Converts Codex JSONL into a compact terminal-style activity feed without discarding the raw log. */
export function parseTerminalLines(content:string,provider:AgentRun['provider']):TerminalLine[]{
  const source=stripAnsi(content).replace(/\r/g,'')
  if(!source.trim())return []
  if(provider!=='codex'){const count=source.split('\n').filter(Boolean).length;return count?[{kind:'system',marker:'›',text:`已接收 ${count} 条终端输出；为保持运行记录简洁，未展示具体内容。`}]:[]}
  const result:TerminalLine[]=[]
  for(const raw of source.split('\n')){
    const text=raw.trim()
    if(!text)continue
    if(!text.startsWith('{')){result.push({kind:'system',marker:'›',text:raw.length>220?'CLI 正在输出较长内容，已省略具体内容。':raw});continue}
    try{
      const event:unknown=JSON.parse(text)
      if(isRecord(event))result.push(...codexEventLines(event))
      else result.push({kind:'output',marker:'│',text:raw})
    }catch{
      // A log refresh can read an incomplete trailing JSON line. Keep it visible but avoid dumping JSONL noise.
      result.push({kind:'working',marker:'…',text:'正在接收 Codex CLI 的实时事件…'})
    }
  }
  return result.length?result:[{kind:'system',marker:'…',text:'正在接收 Codex CLI 的运行事件…'}]
}
