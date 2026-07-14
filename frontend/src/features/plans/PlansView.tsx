import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../../api/client'
import type { CLIProvider, Plan, PlanTask, Project } from '../../api/types'
import { Check, Play } from '../../components/Icons'
import { Empty, relative, Status } from '../../components/Status'

type ProviderChoice=CLIProvider|undefined

const providerOptions:{value:ProviderChoice;label:string;description:string}[]=[
  {value:undefined,label:'项目默认',description:'由服务端读取当前设置'},
  {value:'codex',label:'Codex CLI',description:'覆盖本次执行'},
  {value:'claude',label:'Claude CLI',description:'覆盖本次执行'},
]

function ExecutionProviderSelector({label,value,onChange,disabled=false,compact=false}:{label:string;value:ProviderChoice;onChange:(provider:ProviderChoice)=>void;disabled?:boolean;compact?:boolean}){
  return <fieldset className={`execution-provider-selector${compact?' compact':''}`} disabled={disabled}>
    <legend>{label}</legend>
    <div className="execution-provider-options" role="group" aria-label={label}>
      {providerOptions.map(option=><button
        type="button"
        aria-label={`${label}：${option.label}`}
        aria-pressed={value===option.value}
        className={value===option.value?'active':''}
        key={option.value??'default'}
        onClick={()=>onChange(option.value)}
      >
        <strong>{option.label}</strong>
        <span>{option.description}</span>
      </button>)}
    </div>
  </fieldset>
}

function isOrdinaryTaskRunnable(tasks:PlanTask[],index:number){
  const task=tasks[index]
  return task.title!=='Final validation'
    && ['pending','failed','cancelled'].includes(task.status)
    && tasks.slice(0,index).every(previous=>previous.status==='succeeded')
}

export function PlansView({project,plans}:{project:Project;plans:Plan[]}) {
  const[selected,setSelected]=useState(plans[0]?.id)
  const[planProvider,setPlanProvider]=useState<ProviderChoice>()
  const[taskProviders,setTaskProviders]=useState<Record<string,ProviderChoice>>({})
  const queryClient=useQueryClient()
  const detail=useQuery({queryKey:['plan',selected],queryFn:()=>api.plan(selected!),enabled:!!selected})
  const refreshPlan=(planId:string)=>Promise.all([
    queryClient.invalidateQueries({queryKey:['plans',project.id]}),
    queryClient.invalidateQueries({queryKey:['plan',planId]}),
  ])
  const runPlan=useMutation({
    mutationFn:({plan,provider}:{plan:Plan;provider?:CLIProvider})=>api.runPlan(plan,provider),
    onSuccess:async(_,variables)=>{
      setPlanProvider(undefined)
      await refreshPlan(variables.plan.id)
    },
    onError:async(_,variables)=>{await refreshPlan(variables.plan.id)},
  })
  const runTask=useMutation({
    mutationFn:({task,provider}:{task:PlanTask;provider?:CLIProvider})=>api.runTask(task,provider),
    onSuccess:async(_,variables)=>{
      setTaskProviders(current=>{
        const next={...current}
        delete next[variables.task.id]
        return next
      })
      await refreshPlan(variables.task.planId)
    },
    onError:async(_,variables)=>{await refreshPlan(variables.task.planId)},
  })

  useEffect(()=>{
    if(selected&&plans.some(plan=>plan.id===selected))return
    setSelected(plans[0]?.id)
  },[plans,selected])
  useEffect(()=>{
    setPlanProvider(undefined)
    setTaskProviders({})
  },[selected])

  const selectPlan=(planId:string)=>setSelected(planId)
  const executionPending=runPlan.isPending||runTask.isPending
  const shownPlan=detail.data?.plan
  const canRunPlan=shownPlan&&['ready','blocked'].includes(shownPlan.status)
  const completedTaskCount=detail.data?.tasks.filter(task=>task.status==='succeeded').length??0
  const planError=runPlan.isError&&runPlan.variables?.plan.id===shownPlan?.id?runPlan.error:undefined

  return <div className="split-page plans-page split-scroll-page">
    <section className="collection split-scroll-collection">
      <header><div><span className="eyebrow">执行中心</span><h1>计划</h1></div></header>
      {plans.length===0?<Empty title="暂无结构化计划" body="计划会根据智能体生成的 PlanSpec JSON 确定性渲染。"/>:<div className="plan-list app-scroll-region">{plans.map(plan=><button key={plan.id} className={selected===plan.id?'selected':''} onClick={()=>selectPlan(plan.id)}><div><Status value={plan.status}/><small>{relative(plan.updatedAt)}</small></div><strong>{plan.title}</strong><span>计划版本 {plan.version}</span></button>)}</div>}
    </section>
    <section
      className={`plan-detail split-scroll-detail${detail.data?'':' app-scroll-region'}`}
      style={detail.data?{display:'flex',flexDirection:'column',overflow:'hidden'}:undefined}
    >{detail.isLoading?<div className="loading">正在加载计划…</div>:detail.data?<>
      <header style={{flex:'0 0 auto'}}><div><span className="eyebrow">结构化交付计划</span><h2>{detail.data.plan.title}</h2><div className="detail-meta"><Status value={detail.data.plan.status}/><span>已完成 {completedTaskCount} / {detail.data.tasks.length}</span></div></div></header>
      {canRunPlan&&<section className="plan-execution-panel" aria-label="运行整份计划" style={{flex:'0 0 auto'}}>
        <div className="execution-panel-main">
          <ExecutionProviderSelector label="整份计划执行提供方" value={planProvider} onChange={setPlanProvider} disabled={executionPending}/>
          <p>显式选择会应用于本次计划排入的所有普通任务及其自动重试；选择“项目默认”时不会固化提供方，也不会影响其他计划。</p>
        </div>
        <button className="button primary plan-run-button" onClick={()=>runPlan.mutate({plan:detail.data.plan,provider:planProvider})} disabled={executionPending}><Play/> {runPlan.isPending?'正在加入队列…':'运行计划'}</button>
        {planError&&<div className="form-error execution-error" role="alert">运行计划失败：{planError.message}。状态已刷新，可调整提供方后重试。</div>}
      </section>}
      <div className="progress" style={{flex:'0 0 auto'}}><i style={{width:`${detail.data.tasks.length?completedTaskCount/detail.data.tasks.length*100:0}%`}}/></div>
      <div
        className="plan-detail-scroll app-scroll-region"
        aria-label="计划内容"
        style={{flex:'1 1 auto',minHeight:0,overflowY:'auto',overscrollBehaviorY:'contain',paddingRight:10,scrollbarGutter:'stable'}}
      >
      <div className="task-track">{detail.data.tasks.map((task,index)=>{
        const isValidation=task.title==='Final validation'
        const canRunTask=isOrdinaryTaskRunnable(detail.data.tasks,index)
        const provider=taskProviders[task.id]
        const taskPending=runTask.isPending&&runTask.variables?.task.id===task.id
        const taskError=runTask.isError&&runTask.variables?.task.id===task.id?runTask.error:undefined
        return <article key={task.id} className={`task task-${task.status}${isValidation?' task-validation':''}`}>
          <div className="task-marker">{task.status==='succeeded'?<Check/>:task.position}</div>
          <div>
            <div className="task-heading"><strong>{task.taskKey} · {task.title}</strong><Status value={task.status}/></div>
            <div className="scope-list">{task.scope.map(path=><code key={path}>{path}</code>)}</div>
            <ul>{task.acceptance.map(item=><li key={item}>{item}</li>)}</ul>
            {canRunTask&&<div className="task-execution-panel">
              <ExecutionProviderSelector compact label={`${task.taskKey} 本次执行提供方`} value={provider} onChange={value=>setTaskProviders(current=>({...current,[task.id]:value}))} disabled={executionPending}/>
              <div className="task-execution-actions">
                <span>只影响这次手动{task.status==='failed'?'重试':'运行'}，不会继承计划或其他任务的选择。</span>
                <button className="button small" onClick={()=>runTask.mutate({task,provider})} disabled={executionPending}><Play/> {taskPending?'正在加入队列…':task.status==='failed'?'重试任务':'运行任务'}</button>
              </div>
              {taskError&&<div className="form-error execution-error" role="alert">{task.status==='failed'?'重试':'运行'}任务失败：{taskError.message}。状态已刷新，可重新选择后再试。</div>}
            </div>}
            {isValidation&&<div className="task-validation-note">最终验证仅使用项目配置的验证命令，不通过 Codex CLI 或 Claude CLI 手动执行。</div>}
          </div>
        </article>
      })}</div>
      <details className="markdown-panel"><summary>查看渲染后的计划文档</summary><div className="markdown"><ReactMarkdown remarkPlugins={[remarkGfm]}>{detail.data.plan.markdown}</ReactMarkdown></div></details>
      </div>
    </>:<Empty title="请选择一个计划" body="查看任务、验收标准和执行状态。"/>}</section>
  </div>
}
