import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api } from '../../api/client'
import type { Plan, Project } from '../../api/types'
import { Check, Play } from '../../components/Icons'
import { Empty, relative, Status } from '../../components/Status'

export function PlansView({project,plans}:{project:Project;plans:Plan[]}) {
  const[selected,setSelected]=useState(plans[0]?.id)
  const queryClient=useQueryClient()
  const detail=useQuery({queryKey:['plan',selected],queryFn:()=>api.plan(selected!),enabled:!!selected})
  const run=useMutation({mutationFn:()=>api.runPlan(detail.data!.plan),onSuccess:()=>{queryClient.invalidateQueries({queryKey:['plans',project.id]});queryClient.invalidateQueries({queryKey:['plan',selected]})}})

  return <div className="split-page plans-page">
    <section className="collection">
      <header><div><span className="eyebrow">执行中心</span><h1>计划</h1></div></header>
      {plans.length===0?<Empty title="暂无结构化计划" body="计划会根据智能体生成的 PlanSpec JSON 确定性渲染。"/>:<div className="plan-list">{plans.map(plan=><button key={plan.id} className={selected===plan.id?'selected':''} onClick={()=>setSelected(plan.id)}><div><Status value={plan.status}/><small>{relative(plan.updatedAt)}</small></div><strong>{plan.title}</strong><span>计划版本 {plan.version}</span></button>)}</div>}
    </section>
    <section className="plan-detail">{detail.isLoading?<div className="loading">正在加载计划…</div>:detail.data?<>
      <header><div><span className="eyebrow">结构化交付计划</span><h2>{detail.data.plan.title}</h2><div className="detail-meta"><Status value={detail.data.plan.status}/><span>已完成 {detail.data.tasks.filter(t=>t.status==='succeeded').length} / {detail.data.tasks.length}</span></div></div>{['ready','blocked'].includes(detail.data.plan.status)&&<button className="button primary" onClick={()=>run.mutate()} disabled={run.isPending}><Play/> {run.isPending?'正在加入队列…':'运行计划'}</button>}</header>
      <div className="progress"><i style={{width:`${detail.data.tasks.length?detail.data.tasks.filter(t=>t.status==='succeeded').length/detail.data.tasks.length*100:0}%`}}/></div>
      <div className="task-track">{detail.data.tasks.map(task=><article key={task.id} className={`task task-${task.status}`}><div className="task-marker">{task.status==='succeeded'?<Check/>:task.position}</div><div><div className="task-heading"><strong>{task.taskKey} · {task.title}</strong><Status value={task.status}/></div><div className="scope-list">{task.scope.map(path=><code key={path}>{path}</code>)}</div><ul>{task.acceptance.map(item=><li key={item}>{item}</li>)}</ul></div></article>)}</div>
      <details className="markdown-panel"><summary>查看渲染后的计划文档</summary><div className="markdown"><ReactMarkdown remarkPlugins={[remarkGfm]}>{detail.data.plan.markdown}</ReactMarkdown></div></details>
    </>:<Empty title="请选择一个计划" body="查看任务、验收标准和执行状态。"/>}</section>
  </div>
}
