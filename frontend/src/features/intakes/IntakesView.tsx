import { useEffect, useRef, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../../api/client'
import type { Intake, Project, RequirementDiscussionMessage } from '../../api/types'
import { Plus, Upload } from '../../components/Icons'
import { Empty, kindLabel, relative, Status } from '../../components/Status'

export function IntakesView({project,intakes}:{project:Project;intakes:Intake[]}) {
  const queryClient=useQueryClient()
  const[selected,setSelected]=useState<string|undefined>(intakes[0]?.id)
  const[creating,setCreating]=useState(false)
  const[title,setTitle]=useState('')
  const[body,setBody]=useState('')
  const[kind,setKind]=useState<'requirement'|'feedback'>('requirement')
  const[discussionOpen,setDiscussionOpen]=useState(false)
  const[discussionMessages,setDiscussionMessages]=useState<RequirementDiscussionMessage[]>([])
  const[discussionInput,setDiscussionInput]=useState('')
  const[discussionProvider,setDiscussionProvider]=useState<'codex'|'claude'>()
  const[discussionReady,setDiscussionReady]=useState(false)
  const fileRef=useRef<HTMLInputElement>(null)
  useEffect(()=>{
    if(selected&&intakes.some(intake=>intake.id===selected))return
    setSelected(intakes[0]?.id)
  },[intakes,selected])
  const item=intakes.find(i=>i.id===selected)
  const invalidate=()=>queryClient.invalidateQueries({queryKey:['intakes',project.id]})
  const resetDiscussion=()=>{
    setDiscussionMessages([])
    setDiscussionInput('')
    setDiscussionProvider(undefined)
    setDiscussionReady(false)
    setDiscussionOpen(false)
  }
  const create=useMutation({
    mutationFn:()=>api.createIntake(project.id,{kind,title,body}),
    onSuccess:result=>{
      invalidate()
      setSelected(result.intake.id)
      setCreating(false)
      setTitle('')
      setBody('')
      resetDiscussion()
    },
  })
  const discuss=useMutation({
    mutationFn:(messages:RequirementDiscussionMessage[])=>api.discussRequirement(project.id,{title,body,messages}),
    onSuccess:(result,messages)=>{
      setDiscussionMessages([...messages,{role:'assistant',content:result.reply}])
      setDiscussionProvider(result.provider)
      setDiscussionReady(result.ready)
      if(result.title)setTitle(result.title)
      if(result.body)setBody(result.body)
    },
  })
  const generate=useMutation({mutationFn:()=>api.generatePlan(item!),onSuccess:invalidate})
  const upload=useMutation({mutationFn:(file:File)=>api.upload(item!.id,file)})

  const beginCreate=()=>{
    setCreating(true)
    setKind('requirement')
  }
  const sendDiscussionMessage=()=>{
    const content=discussionInput.trim()
    if(!content||discuss.isPending)return
    const messages:RequirementDiscussionMessage[]=[...discussionMessages,{role:'user',content}]
    setDiscussionMessages(messages)
    setDiscussionInput('')
    setDiscussionReady(false)
    discuss.mutate(messages)
  }
  const providerLabel=discussionProvider==='claude'?'Claude':discussionProvider==='codex'?'Codex':'本地 CLI'

  return <div className="split-page">
    <section className="collection">
      <header><div><span className="eyebrow">输入队列</span><h1>需求与反馈</h1></div><button className="button primary small" onClick={beginCreate}><Plus/> 新建需求</button></header>
      <div className="filter-row"><button className="chip active">全部 <b>{intakes.length}</b></button><button className="chip">需求</button><button className="chip">反馈</button></div>
      {intakes.length===0?<Empty title="输入队列为空" body="添加需求或反馈，开始自动交付流程。" action={<button className="button primary" onClick={beginCreate}>创建需求</button>}/>:<div className="intake-list">{intakes.map(intake=><button key={intake.id} className={intake.id===selected?'selected':''} onClick={()=>{setSelected(intake.id);setCreating(false)}}><div><span className={`kind kind-${intake.kind}`}>{kindLabel(intake.kind)}</span><Status value={intake.status}/></div><strong>{intake.title}</strong><p>{intake.body||'暂无说明'}</p><small>{relative(intake.updatedAt)}</small></button>)}</div>}
    </section>
    <section className="editor">{creating?<form onSubmit={e=>{e.preventDefault();create.mutate()}}>
      <header>
        <div><span className="eyebrow">新建输入</span><h2>定义工作内容</h2></div>
        <div className="segmented"><button type="button" className={kind==='requirement'?'active':''} onClick={()=>setKind('requirement')}>需求</button><button type="button" className={kind==='feedback'?'active':''} onClick={()=>setKind('feedback')}>反馈</button></div>
      </header>

      {kind==='requirement'&&<section className={`discussion-panel ${discussionOpen?'open':''}`}>
        <div className="discussion-header">
          <div>
            <div className="discussion-title"><strong>与本地 CLI 讨论需求</strong><span className="discussion-badge">只读分析</span></div>
            <p>使用项目当前配置的 Codex 或 Claude，读取本地代码上下文并帮助澄清需求，不会修改项目文件。</p>
          </div>
          <button type="button" className="button secondary small" onClick={()=>setDiscussionOpen(value=>!value)}>{discussionOpen?'收起讨论':'开始讨论'}</button>
        </div>
        {discussionOpen&&<div className="discussion-content">
          <div className="discussion-messages" aria-live="polite">
            {discussionMessages.length===0?<div className="discussion-empty"><strong>先说说你想实现什么</strong><span>CLI 会结合当前项目提出澄清问题，并逐步整理标题、需求说明和验收标准。</span></div>:discussionMessages.map((message,index)=><div className={`discussion-message ${message.role}`} key={`${message.role}-${index}`}>
              <div className="discussion-message-meta">{message.role==='user'?'你':providerLabel}</div>
              <div>{message.content}</div>
            </div>)}
            {discuss.isPending&&<div className="discussion-message assistant pending"><div className="discussion-message-meta">{providerLabel}</div><div><span className="discussion-pulse"/>正在只读分析本地项目并整理需求，请稍候…</div></div>}
          </div>
          {discussionReady&&<div className="discussion-ready"><strong>需求已经足够明确</strong><span>CLI 整理的标题和说明已写入下方表单。确认内容后即可创建需求{project.automationEnabled?'并自动生成计划':'。'}。</span></div>}
          {discuss.error&&<div className="form-error">CLI 讨论失败：{discuss.error.message}</div>}
          <div className="discussion-composer">
            <textarea
              value={discussionInput}
              onChange={event=>setDiscussionInput(event.target.value)}
              onKeyDown={event=>{if((event.ctrlKey||event.metaKey)&&event.key==='Enter'){event.preventDefault();sendDiscussionMessage()}}}
              placeholder="描述你的想法，或回答 CLI 提出的澄清问题…"
              rows={3}
              disabled={discuss.isPending}
            />
            <div><span>按 Ctrl/⌘ + Enter 发送</span><button type="button" className="button primary" onClick={sendDiscussionMessage} disabled={!discussionInput.trim()||discuss.isPending}>{discuss.isPending?'分析中…':'发送给 CLI'}</button></div>
          </div>
        </div>}
      </section>}

      <label><span>标题</span><input autoFocus={!discussionOpen} value={title} onChange={e=>setTitle(e.target.value)} placeholder="希望做出什么改变？" required/></label>
      <label className="grow"><span>详细说明</span><textarea value={body} onChange={e=>setBody(e.target.value)} placeholder="请描述背景、约束条件和期望结果…" required/></label>
      {create.error&&<div className="form-error">提交失败：{create.error.message}</div>}
      <footer><button type="button" className="button ghost" onClick={()=>{setCreating(false);resetDiscussion()}}>取消</button><button className="button primary" disabled={create.isPending||discuss.isPending}>{create.isPending?'正在提交…':project.automationEnabled?'提交并生成计划':'保存需求'}</button></footer>
    </form>:item?<article className="intake-detail">
      <header><div><div className="detail-meta"><span className={`kind kind-${item.kind}`}>{kindLabel(item.kind)}</span><Status value={item.status}/></div><h2>{item.title}</h2><small>创建于 {new Date(item.createdAt).toLocaleString('zh-CN')}</small></div></header>
      <div className="intake-body">{item.body}</div>
      <div className="attachment-box" onClick={()=>fileRef.current?.click()}><Upload/><div><strong>添加上下文附件</strong><span>支持图片、日志和参考文件，单个文件最大 50 MiB</span></div><input ref={fileRef} type="file" hidden onChange={e=>{const file=e.target.files?.[0];if(file)upload.mutate(file)}}/></div>
      {upload.error&&<div className="form-error">上传失败：{upload.error.message}</div>}
      <footer><button className="button secondary" onClick={()=>generate.mutate()} disabled={generate.isPending||item.status==='planning'}>{generate.isPending?'正在加入队列…':'生成计划'}</button></footer>
    </article>:<Empty title="请选择一条需求" body="从左侧选择项目，查看详细内容。"/>}</section>
  </div>
}
