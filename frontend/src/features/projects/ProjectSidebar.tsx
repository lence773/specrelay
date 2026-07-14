import type { Project } from '../../api/types'
import { Folder, Logo, Plus } from '../../components/Icons'

export function ProjectSidebar({projects,selected,onSelect,onCreate}:{projects:Project[];selected?:string;onSelect:(id:string)=>void;onCreate:()=>void}) {
  return <aside className="sidebar">
    <div className="sidebar-header">
      <div className="brand" data-tauri-drag-region><Logo/><div data-tauri-drag-region><strong data-tauri-drag-region>SpecRelay</strong><span data-tauri-drag-region>智能体工作区</span></div></div>
      <div className="side-heading"><span>项目</span><button className="icon-button" onClick={onCreate} title="新建项目" aria-label="新建项目"><Plus/></button></div>
    </div>
    <div className="project-list">{projects.map(project=><button key={project.id} className={`project-link ${selected===project.id?'selected':''}`} onClick={()=>onSelect(project.id)}><span className="project-icon"><Folder/></span><span><strong>{project.name}</strong><small>{project.automationEnabled?'自动化已开启':'自动化已暂停'}</small></span><i className={project.automationEnabled?'online':''}/></button>)}</div>
    <div className="sidebar-foot"><span className="health-dot"/> 本地服务 <kbd>v1</kbd></div>
  </aside>
}
