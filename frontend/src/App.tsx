import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './api/client'
import { subscribe } from './api/events'
import type { EventRecord } from './api/types'
import { Activity, Dashboard, Inbox, PlanIcon, Play, SettingsIcon, Stop } from './components/Icons'
import { Empty } from './components/Status'
import { EventsView } from './features/events/EventsView'
import { IntakesView } from './features/intakes/IntakesView'
import { PlansView } from './features/plans/PlansView'
import { RunsView } from './features/runs/RunsView'
import { NewProjectModal } from './features/projects/NewProjectModal'
import { Overview } from './features/projects/Overview'
import { ProjectSidebar } from './features/projects/ProjectSidebar'
import { SettingsView } from './features/settings/SettingsView'

type Tab='overview'|'intakes'|'plans'|'runs'|'events'|'settings'
const EVENT_PAGE_LIMIT=100

const nav:[Tab,string,React.ReactNode][]=[
  ['overview','概览',<Dashboard/>],
  ['intakes','需求',<Inbox/>],
  ['plans','计划与任务',<PlanIcon/>],
  ['runs','CLI 运行',<Activity/>],
  ['events','事件',<Activity/>],
  ['settings','设置',<SettingsIcon/>],
]

export function App(){
  const queryClient=useQueryClient()
  const[authReady,setAuthReady]=useState(false)
  const[authError,setAuthError]=useState('')
  const[selected,setSelected]=useState<string>()
  const[tab,setTab]=useState<Tab>('overview')
  const[createOpen,setCreateOpen]=useState(false)
  const[recentEvents,setRecentEvents]=useState<EventRecord[]>([])

  useEffect(()=>{
    const url=new URL(window.location.href)
    const token=url.searchParams.get('token')
    if(!token){setAuthReady(true);return}
    api.exchange(token).then(()=>{
      url.searchParams.delete('token')
      history.replaceState({},'',url)
      setAuthReady(true)
    }).catch(error=>setAuthError(error.message))
  },[])

  const projects=useQuery({queryKey:['projects'],queryFn:api.projects,enabled:authReady,retry:false})
  useEffect(()=>{if(!selected&&projects.data?.[0])setSelected(projects.data[0].id)},[projects.data,selected])
  const project=projects.data?.find(p=>p.id===selected)
  const intakes=useQuery({queryKey:['intakes',selected],queryFn:()=>api.intakes(selected!),enabled:!!selected})
  const plans=useQuery({queryKey:['plans',selected],queryFn:()=>api.plans(selected!),enabled:!!selected})
  const eventPage=useQuery({queryKey:['events',selected],queryFn:()=>api.events(selected!,undefined,EVENT_PAGE_LIMIT),enabled:!!selected})
  useEffect(()=>{setRecentEvents([])},[selected])
  useEffect(()=>{if(eventPage.data)setRecentEvents(eventPage.data.items.filter(event=>event.type!=='agent.output'))},[eventPage.data])
  useEffect(()=>{
    if(!selected||!eventPage.isSuccess)return
    const after=eventPage.data.items.find(event=>event.type!=='agent.output')?.id??0
    return subscribe(selected,after,queryClient,event=>setRecentEvents(current=>[event,...current.filter(item=>item.id!==event.id)].slice(0,500)))
  },[selected,eventPage.isSuccess,eventPage.data,queryClient])
  const automation=useMutation({mutationFn:(enabled:boolean)=>api.automation(project!,enabled),onSuccess:()=>queryClient.invalidateQueries({queryKey:['projects']})})
  const counts=useMemo(()=>({
    intakes:intakes.data?.filter(i=>i.status==='open'||i.status==='plan_failed').length??0,
    plans:plans.data?.filter(p=>p.status==='running'||p.status==='blocked').length??0,
  }),[intakes.data,plans.data])

  if(authError)return <div className="auth-screen"><div className="auth-card"><span>身份验证失败</span><h1>请重新打开一次性登录链接。</h1><p>{authError}</p></div></div>
  if(!authReady||projects.isLoading)return <div className="splash"><div className="splash-logo">S</div><span>正在启动 SpecRelay…</span></div>

  return <div className="app-shell">
    <ProjectSidebar projects={projects.data??[]} selected={selected} onSelect={id=>{setSelected(id);setTab('overview')}} onCreate={()=>setCreateOpen(true)}/>
    <main className={project?'workspace-main':undefined}>{project?<>
      <header className="topbar">
        <nav>{nav.map(([id,label,icon])=><button key={id} className={tab===id?'active':''} onClick={()=>setTab(id)}>{icon}<span>{label}</span>{id==='intakes'&&counts.intakes>0&&<b>{counts.intakes}</b>}{id==='plans'&&counts.plans>0&&<b>{counts.plans}</b>}</button>)}</nav>
        <button className={`automation ${project.automationEnabled?'running':''}`} disabled={automation.isPending} title={project.automationEnabled ? '停止后会取消排队中的自动任务' : '启动后会自动生成计划，并执行所有已就绪计划'} onClick={()=>automation.mutate(!project.automationEnabled)}>{project.automationEnabled?<><Stop/> 停止自动化</>:<><Play/> 启动自动化</>}</button>
      </header>
      <div className="content">
        {tab==='overview'&&<Overview project={project} intakes={intakes.data??[]} plans={plans.data??[]} events={recentEvents} onNavigate={value=>setTab(value as Tab)}/>}
        <div hidden={tab!=='intakes'}><IntakesView key={project.id} project={project} intakes={intakes.data??[]}/></div>
        {tab==='plans'&&<PlansView project={project} plans={plans.data??[]}/>} 
        {tab==='runs'&&<RunsView project={project}/>} 
        {tab==='events'&&<EventsView events={[...(eventPage.data?.items??[])].reverse()}/>}
        {tab==='settings'&&<SettingsView project={project}/>} 
      </div>
    </>:<div className="welcome"><Empty title="创建第一个项目" body="将 SpecRelay 绑定到可信的本地工作目录，配置智能体，然后把需求转化为经过验证的代码。" action={<button className="button primary" onClick={()=>setCreateOpen(true)}>创建项目</button>}/></div>}</main>
    {createOpen&&<NewProjectModal onClose={()=>setCreateOpen(false)} onCreated={id=>{setSelected(id);setCreateOpen(false)}}/>}
  </div>
}
