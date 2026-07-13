import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../api/client'
import type { Project, ProjectSettings } from '../../api/types'

export function SettingsView({project}:{project:Project}) {
  const queryClient=useQueryClient()
  const query=useQuery({queryKey:['settings',project.id],queryFn:()=>api.settings(project.id)})
  const[form,setForm]=useState<ProjectSettings>()
  useEffect(()=>{if(query.data)setForm(query.data)},[query.data])
  const save=useMutation({mutationFn:()=>api.updateSettings(form!),onSuccess:data=>{setForm(data);queryClient.setQueryData(['settings',project.id],data)}})
  const probe=useMutation({mutationFn:()=>api.probe(project.id)})
  if(!form)return <div className="loading">正在加载设置…</div>
  const patch=(values:Partial<ProjectSettings>)=>setForm({...form,...values})

  return <div className="page narrow settings-page">
    <section className="hero compact"><div><span className="eyebrow">项目配置</span><h1>自动化设置</h1><p>命令只会在 <code>{project.workspacePath}</code> 目录内执行。</p></div><button className="button primary" onClick={()=>save.mutate()} disabled={save.isPending}>{save.isPending?'正在保存…':'保存更改'}</button></section>
    <div className="settings-grid">
      <section className="panel settings-section">
        <header><div><span className="eyebrow">智能体适配器</span><h2>服务提供方</h2></div><button className="button ghost small" onClick={()=>probe.mutate()} disabled={probe.isPending}>{probe.isPending?'正在检测…':'检测本地 CLI'}</button></header>
        <div className="provider-picker"><button className={form.agentProvider==='codex'?'active':''} onClick={()=>patch({agentProvider:'codex'})}><strong>Codex CLI</strong><span>使用结构化 JSON 执行</span></button><button className={form.agentProvider==='claude'?'active':''} onClick={()=>patch({agentProvider:'claude'})}><strong>Claude CLI</strong><span>使用打印模式自动化</span></button></div>
        <div className="field-grid"><label><span>Codex 命令</span><input value={form.codexCommand} onChange={e=>patch({codexCommand:e.target.value})}/></label><label><span>Codex 参数</span><input value={form.codexArgs.join(' ')} onChange={e=>patch({codexArgs:e.target.value.split(/\s+/).filter(Boolean)})}/></label><label><span>Claude 命令</span><input value={form.claudeCommand} onChange={e=>patch({claudeCommand:e.target.value})}/></label><label><span>Claude 参数</span><input value={form.claudeArgs.join(' ')} onChange={e=>patch({claudeArgs:e.target.value.split(/\s+/).filter(Boolean)})}/></label></div>
        {probe.data&&<pre className="probe-output">{probe.data.output||`进程退出码：${probe.data.exitCode}`}</pre>}
        {probe.error&&<div className="form-error">CLI 检测失败：{probe.error.message}</div>}
      </section>
      <section className="panel settings-section">
        <header><div><span className="eyebrow">安全限制</span><h2>执行设置</h2></div></header>
        <label><span>最终验证命令</span><input value={form.validationCommand} onChange={e=>patch({validationCommand:e.target.value})} placeholder="npm test && npm run build"/><small>这是唯一会通过 Shell 执行的项目命令。</small></label>
        <div className="no-timeout-note"><i/><div><strong>CLI 执行不设置超时时间</strong><span>计划生成、需求讨论、任务执行和最终验证会持续运行，直到自然结束或由你停止自动化、计划或任务。可在“CLI 运行”页面实时查看输出。</span></div></div>
        <div className="field-grid"><label><span>最大重试次数</span><input type="number" value={form.maxRetries} onChange={e=>patch({maxRetries:Number(e.target.value)})}/></label></div>
        <label><span>允许传入的环境变量名</span><input value={form.allowedEnv.join(', ')} onChange={e=>patch({allowedEnv:e.target.value.split(',').map(x=>x.trim()).filter(Boolean)})} placeholder="OPENAI_API_KEY, ANTHROPIC_API_KEY"/><small>变量值只保存在服务端，API 不会将其返回。</small></label>
      </section>
    </div>
    {save.error&&<div className="form-error">保存失败：{save.error.message}</div>}
  </div>
}
