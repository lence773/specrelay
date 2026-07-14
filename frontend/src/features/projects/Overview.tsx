import type { EventRecord, Intake, Plan, Project } from '../../api/types'
import { Activity, Check, Inbox, PlanIcon } from '../../components/Icons'
import { Empty, eventLabel, relative, resourceLabel, Status } from '../../components/Status'

export function Overview({project,intakes,plans,events,onNavigate}:{project:Project;intakes:Intake[];plans:Plan[];events:EventRecord[];onNavigate:(tab:string)=>void}) {
  const active=plans.filter(p=>['running','validating'].includes(p.status)).length
  const completed=plans.filter(p=>p.status==='completed').length
  const visibleEvents=events.filter(event=>event.type!=='agent.output')
  return <div className="page">
    <section className="hero">
      <div><span className="eyebrow">工作区概览</span><h1>{project.name}</h1><p>{project.description||'从需求出发，自动交付经过验证的代码。'}</p></div>
      <div className="workspace-pill"><span>工作目录</span><strong>{project.workspacePath}</strong></div>
    </section>
    <section className="metric-grid">
      <Metric icon={<Inbox/>} label="待处理需求" value={intakes.filter(i=>!['closed','planned'].includes(i.status)).length}/>
      <Metric icon={<PlanIcon/>} label="进行中计划" value={active}/>
      <Metric icon={<Check/>} label="已完成计划" value={completed}/>
      <Metric icon={<Activity/>} label="今日事件" value={visibleEvents.filter(e=>new Date(e.occurredAt).toDateString()===new Date().toDateString()).length}/>
    </section>
    <div className="two-column">
      <section className="panel">
        <header className="panel-header"><div><span className="eyebrow">交付队列</span><h2>最近计划</h2></div><button className="text-button" onClick={()=>onNavigate('plans')}>查看全部 →</button></header>
        {plans.length===0?<Empty title="暂无计划" body="创建一条需求并生成第一个结构化计划。"/>:<div className="rows">{plans.slice(0,5).map(plan=><div className="row" key={plan.id}><div className="row-symbol">{plan.status==='completed'?'✓':plan.status==='running'?'▶':'◇'}</div><div className="row-main"><strong>{plan.title}</strong><small>更新于 {relative(plan.updatedAt)}</small></div><Status value={plan.status}/></div>)}</div>}
      </section>
      <section className="panel">
        <header className="panel-header"><div><span className="eyebrow">实时动态</span><h2>最新活动</h2></div><span className="live-label"><i/> 实时</span></header>
        {visibleEvents.length===0?<Empty title="暂无活动" body="领域事件和智能体执行进度会显示在这里。"/>:<div className="timeline">{visibleEvents.slice(0,6).map(event=><div className="timeline-item" key={event.id}><i/><div><strong>{eventLabel(event.type)}</strong><small>{relative(event.occurredAt)} · {resourceLabel(event.aggregateType)}</small></div></div>)}</div>}
      </section>
    </div>
  </div>
}

function Metric({icon,label,value}:{icon:React.ReactNode;label:string;value:number}) {
  return <article className="metric"><span className="metric-icon">{icon}</span><div><strong>{value}</strong><span>{label}</span></div></article>
}
