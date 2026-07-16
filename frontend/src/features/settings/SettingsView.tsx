import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../api/client'
import type { McpConnectionInfo, McpDiagnostic, Project, ProjectSettings } from '../../api/types'
import { Modal } from '../../components/Modal'

type TauriBridge={core?:{invoke:(command:string,payload?:Record<string,unknown>)=>Promise<unknown>}}

type MCPNotice={kind:'success'|'error';message:string}

function mcpAddress(endpointPath:string){return new URL(endpointPath,window.location.origin).toString()}

function clientConfiguration(connection:McpConnectionInfo,address:string,token?:string){
  return JSON.stringify({
    mcpServers:{
      specrelay:{
        url:address,
        transport:connection.transport,
        headers:{Authorization:`Bearer ${token??'<在此填写 MCP Bearer Token>'}`},
      },
    },
  },null,2)
}

function checkedAt(value:string){
  const date=new Date(value)
  return Number.isNaN(date.getTime())?value:date.toLocaleString('zh-CN',{dateStyle:'medium',timeStyle:'medium'})
}

function MCPConnection(){
  const queryClient=useQueryClient()
  const connection=useQuery({queryKey:['mcp','connection'],queryFn:api.mcpConnection})
  const[latestDiagnostic,setLatestDiagnostic]=useState<McpDiagnostic>()
  const diagnostic=useMutation({
    mutationFn:api.diagnoseMcp,
    onSuccess:setLatestDiagnostic,
    onError:()=>setLatestDiagnostic({success:false,checkedAt:new Date().toISOString(),failure:'无法完成 MCP 服务检测，请稍后重试。'}),
  })
  const[notice,setNotice]=useState<MCPNotice>()
  const[rotationConfirmationOpen,setRotationConfirmationOpen]=useState(false)
  const[rotationPending,setRotationPending]=useState(false)
  const[rotationError,setRotationError]=useState<string>()
  const[rotatedToken,setRotatedToken]=useState<string>()

  const copy=async(value:string,success:string,failure:string)=>{
    try{
      if(!navigator.clipboard?.writeText)throw new Error('clipboard_unavailable')
      await navigator.clipboard.writeText(value)
      setNotice({kind:'success',message:success})
    }catch{
      setNotice({kind:'error',message:failure})
    }
  }
  const startRotation=()=>{
    setRotationError(undefined)
    setRotationConfirmationOpen(true)
  }
  const closeRotationConfirmation=()=>{
    if(!rotationPending)setRotationConfirmationOpen(false)
  }
  const rotate=async()=>{
    setRotationPending(true)
    setRotationError(undefined)
    try{
      const result=await api.rotateMcpToken()
      setRotationConfirmationOpen(false)
      setRotatedToken(result.token)
      setNotice({kind:'success',message:'已生成新的 MCP Token。请立即复制并安全保存。'})
      void queryClient.invalidateQueries({queryKey:['mcp','connection']})
    }catch{
      setRotationError('无法轮换 MCP Token。旧 Token 未在页面中回显，请稍后重试。')
    }finally{
      setRotationPending(false)
    }
  }
  const closeTokenResult=()=>setRotatedToken(undefined)

  if(connection.isLoading)return <section className="panel settings-section mcp-connection-section"><header><div><span className="eyebrow">MCP 集成</span><h2>MCP 连接</h2></div></header><div className="loading mcp-loading">正在加载 MCP 连接信息…</div></section>
  if(connection.error||!connection.data)return <section className="panel settings-section mcp-connection-section"><header><div><span className="eyebrow">MCP 集成</span><h2>MCP 连接</h2></div><button className="button ghost small" onClick={()=>connection.refetch()}>重新加载</button></header><div className="form-error">无法加载 MCP 连接信息。请检查登录状态和服务是否可用后重试。</div></section>

  const info=connection.data
  const address=mcpAddress(info.endpointPath)
  const configured=info.token.state==='configured'
  const diagnosticFailure=latestDiagnostic&&!latestDiagnostic.success

  return <>
    <section className="panel settings-section mcp-connection-section">
      <header><div><span className="eyebrow">MCP 集成</span><h2>MCP 连接</h2></div><div className="mcp-header-actions"><button className="button ghost small" onClick={()=>diagnostic.mutate()} disabled={diagnostic.isPending}>{diagnostic.isPending?'正在检测…':'检测连接'}</button><button className="button danger small" onClick={startRotation}>轮换 Token</button></div></header>
      <p className="settings-description">使用以下信息将当前 SpecRelay 服务添加到 MCP 客户端。连接地址会根据当前浏览器访问地址生成，兼容开发代理和桌面端。</p>
      <div className="mcp-connection-details">
        <div className="mcp-address-card"><span>服务地址</span><code>{address}</code><button className="button ghost small" onClick={()=>copy(address,'已复制 MCP 服务地址。','无法复制 MCP 服务地址。请手动复制地址。')}>复制连接地址</button></div>
        <dl className="mcp-facts">
          <div><dt>传输方式</dt><dd>{info.transport==='streamable-http'?'Streamable HTTP':info.transport}</dd></div>
          <div><dt>鉴权方式</dt><dd>{info.authentication.scheme==='bearer'?'Bearer Token':info.authentication.scheme}</dd></div>
          <div><dt>Token 状态</dt><dd><span className={`mcp-token-status ${configured?'configured':'unconfigured'}`}>{configured?'已配置（不回显）':'未配置'}</span></dd></div>
          <div><dt>服务版本</dt><dd>{info.serviceName} {info.serviceVersion}{info.protocolVersion&&` · 协议 ${info.protocolVersion}`}</dd></div>
        </dl>
      </div>
      <div className="mcp-actions"><button className="button secondary" onClick={()=>copy(clientConfiguration(info,address),'已复制 MCP 客户端配置。','无法复制 MCP 客户端配置。请手动复制配置。')}>复制客户端配置</button><small>未刚刚轮换 Token 时，配置只包含明确的 Token 占位符，不会读取或回显当前 Token。</small></div>
      {notice&&<div className={`mcp-notice ${notice.kind}`} role="status">{notice.message}</div>}
      <div className="mcp-diagnostic" aria-live="polite"><div><strong>服务可用性</strong><span>仅检测 SpecRelay MCP 服务端点与处理链，不表示外部 MCP 客户端已连接。</span></div>{!latestDiagnostic?<span className="mcp-diagnostic-state">尚未检测</span>:<div className={`mcp-diagnostic-result ${latestDiagnostic.success?'success':'failure'}`}><strong>{latestDiagnostic.success?'检测成功':'检测失败'}</strong><span>{checkedAt(latestDiagnostic.checkedAt)}</span>{diagnosticFailure&&<small>{latestDiagnostic.failure||'MCP 服务当前不可用。'}</small>}</div>}</div>
      <div className="mcp-tools"><div><strong>MCP 工具</strong><span>当前服务实际暴露的工具</span></div><ul>{info.tools.map(tool=><li key={tool.name}><code>{tool.name}</code><span>{tool.description}</span></li>)}</ul></div>
    </section>
    {rotationConfirmationOpen&&<Modal title="确认轮换 MCP Token" onClose={closeRotationConfirmation} closeDisabled={rotationPending} className="mcp-rotation-modal">
      <div className="mcp-rotation-dialog"><p>轮换后，旧 MCP Token 会立即失效。所有仍使用旧 Token 的 MCP 客户端都需要更新配置。</p><p>当前 Token 不会显示或复制到此页面。</p>{rotationError&&<div className="form-error">{rotationError}</div>}<footer className="modal-actions"><button className="button ghost" onClick={closeRotationConfirmation} disabled={rotationPending}>取消</button><button className="button danger" onClick={()=>void rotate()} disabled={rotationPending}>{rotationPending?'正在轮换…':'确认轮换并生成新 Token'}</button></footer></div>
    </Modal>}
    {rotatedToken&&<Modal title="新的 MCP Token" onClose={closeTokenResult} className="mcp-token-result-modal">
      <div className="mcp-token-result"><p>新 Token 仅在当前结果界面显示一次。关闭此窗口或离开设置页后，无法再次查看。</p><code>{rotatedToken}</code><div className="mcp-result-actions"><button className="button secondary" onClick={()=>copy(rotatedToken,'已复制新的 MCP Token。','无法复制新的 MCP Token。请手动复制。')}>复制新 Token</button><button className="button primary" onClick={()=>copy(clientConfiguration(info,address,rotatedToken),'已复制包含新 Token 的客户端配置。','无法复制客户端配置。请手动复制。')}>复制包含新 Token 的配置</button></div><footer className="modal-actions"><button className="button ghost" onClick={closeTokenResult}>完成并关闭</button></footer></div>
    </Modal>}
  </>
}

function DesktopDatabaseConnection(){
  const bridge=(window as Window&{__TAURI__?:TauriBridge}).__TAURI__
  const available=typeof bridge?.core?.invoke==='function'
  const[confirmationOpen,setConfirmationOpen]=useState(false)
  const configure=useMutation({
    mutationFn:async()=>{
      if(!bridge?.core?.invoke)throw new Error('数据库连接只能在 SpecRelay 桌面端中修改。')
      await bridge.core.invoke('open_database_configuration')
    },
    onSuccess:()=>setConfirmationOpen(false),
  })
  const requestConfiguration=()=>{
    configure.reset()
    setConfirmationOpen(true)
  }
  const closeConfirmation=()=>{
    if(!configure.isPending)setConfirmationOpen(false)
  }

  return <>
    <section className="panel settings-section database-connection-section">
      <header><div><span className="eyebrow">桌面端数据连接</span><h2>PostgreSQL 数据库</h2></div>{available&&<button className="button ghost small" onClick={requestConfiguration} disabled={configure.isPending}>{configure.isPending?'正在打开设置…':'修改连接'}</button>}</header>
      {available?<>
        <p className="settings-description">在这里更新此桌面端使用的数据库主机、端口、数据库名、用户名、密码和 SSL 模式。已保存的密码不会回显。</p>
        <div className="database-connection-note"><i/><div><strong>安全切换连接</strong><span>开始修改后，桌面端只会安全停止自身启动的本机后端和 CLI 进程，写入当前任务状态；不会停止、删除或修改 PostgreSQL、Docker 容器或其他应用。</span></div></div>
        {configure.error&&<div className="form-error">无法打开数据库连接设置：{configure.error.message}</div>}
      </>:<div className="database-connection-note"><i/><div><strong>仅桌面端可用</strong><span>当前是浏览器访问模式。请在 SpecRelay 桌面安装包中打开“设置”，即可修改数据连接。</span></div></div>}
    </section>
    {confirmationOpen&&<Modal title="准备修改数据库连接" onClose={closeConfirmation} className="database-reconfigure-modal" closeDisabled={configure.isPending}>
      <div className="database-reconfigure-dialog">
        <div className="database-reconfigure-hero">
          <span className="database-reconfigure-icon" aria-hidden="true">⇄</span>
          <div><strong>先安全暂停本机服务</strong><p>修改前会保存当前执行状态，然后进入数据库连接设置页。</p></div>
        </div>
        <div className="database-reconfigure-impact" aria-label="本次操作范围">
          <div className="safe"><span aria-hidden="true">✓</span><div><strong>将安全停止</strong><small>当前桌面端启动的本机后端和 CLI 任务。</small></div></div>
          <div className="safe"><span aria-hidden="true">✓</span><div><strong>将保留</strong><small>当前任务状态；重新连接成功后可继续恢复处理。</small></div></div>
          <div className="protected"><span aria-hidden="true">⊘</span><div><strong>不会触碰</strong><small>PostgreSQL、Docker 容器及其他应用进程。</small></div></div>
        </div>
        <p className="database-reconfigure-tip">连接信息保存成功后，桌面端会自动检查数据库结构并启动本机服务。密码始终不会回显。</p>
        {configure.error&&<div className="form-error">无法打开数据库连接设置：{configure.error.message}</div>}
        <footer className="modal-actions database-reconfigure-actions">
          <button type="button" className="button ghost" onClick={closeConfirmation} disabled={configure.isPending}>暂不修改</button>
          <button type="button" className="button primary" onClick={()=>configure.mutate()} disabled={configure.isPending}>{configure.isPending?'正在安全切换…':'继续修改连接'}</button>
        </footer>
      </div>
    </Modal>}
  </>
}

export function SettingsView({project}:{project?:Project}) {
  const queryClient=useQueryClient()
  const query=useQuery({queryKey:['settings',project?.id],queryFn:()=>api.settings(project!.id),enabled:!!project})
  const[form,setForm]=useState<ProjectSettings>()
  useEffect(()=>{if(query.data)setForm(query.data)},[query.data])
  const save=useMutation({mutationFn:()=>api.updateSettings(form!),onSuccess:data=>{setForm(data);if(project)queryClient.setQueryData(['settings',project.id],data)}})
  const probe=useMutation({mutationFn:()=>api.probe(project!.id)})
  const patch=(values:Partial<ProjectSettings>)=>setForm(current=>current?{...current,...values}:current)

  return <div className="page narrow settings-page">
    <section className="hero compact"><div><span className="eyebrow">{project?'项目配置':'桌面端配置'}</span><h1>{project?'自动化设置':'设置'}</h1><p>{project?<>命令只会在 <code>{project.workspacePath}</code> 目录内执行。</>:'可在此管理 MCP 接入和桌面端的数据连接；创建项目后，还可配置本地 CLI 与自动化执行。'}</p></div>{project&&form&&<button className="button primary" onClick={()=>save.mutate()} disabled={save.isPending}>{save.isPending?'正在保存…':'保存更改'}</button>}</section>
    <div className="settings-grid">
      <MCPConnection/>
      <DesktopDatabaseConnection/>
      {!project?<section className="panel settings-section"><header><div><span className="eyebrow">项目配置</span><h2>尚未选择项目</h2></div></header><p className="settings-description">创建或选择一个项目后，可在此检测本地 Codex / Claude CLI、设置执行参数和自动化策略。</p></section>:!form?<div className="loading">正在加载项目设置…</div>:<>
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
      </>}
    </div>
    {save.error&&<div className="form-error">保存失败：{save.error.message}</div>}
  </div>
}
