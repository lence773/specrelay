import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../../api/client'
import type { DirectoryListing } from '../../api/types'
import { Modal } from '../../components/Modal'

export function NewProjectModal({onClose,onCreated}:{onClose:()=>void;onCreated:(id:string)=>void}) {
  const queryClient=useQueryClient()
  const [name,setName]=useState('')
  const [workspacePath,setWorkspacePath]=useState('')
  const [description,setDescription]=useState('')
  const [browserOpen,setBrowserOpen]=useState(false)
  const [listing,setListing]=useState<DirectoryListing>()
  const [browseLoading,setBrowseLoading]=useState(false)
  const [browseError,setBrowseError]=useState('')
  const mutation=useMutation({
    mutationFn:api.createProject,
    onSuccess:project=>{
      queryClient.invalidateQueries({queryKey:['projects']})
      onCreated(project.id)
    },
  })

  async function browse(path?:string) {
    setBrowserOpen(true)
    setBrowseLoading(true)
    setBrowseError('')
    try {
      setListing(await api.directories(path?.trim()||undefined))
    } catch(error) {
      setBrowseError(error instanceof Error?error.message:'无法打开目录')
    } finally {
      setBrowseLoading(false)
    }
  }

  function chooseCurrentDirectory() {
    if(!listing)return
    setWorkspacePath(listing.path)
    if(!name)setName(listing.path.split('/').filter(Boolean).at(-1)??listing.path)
    setBrowserOpen(false)
  }

  return <Modal title="创建项目" onClose={onClose} wide={browserOpen}>
    <form className="form-stack" onSubmit={e=>{e.preventDefault();mutation.mutate({name,workspacePath,description})}}>
      <label>
        <span>项目名称</span>
        <input autoFocus value={name} onChange={e=>setName(e.target.value)} placeholder="例如：Atlas API" required/>
      </label>
      <div className="form-field">
        <span className="field-label">工作目录路径</span>
        <div className="path-input-row">
          <input value={workspacePath} onChange={e=>setWorkspacePath(e.target.value)} placeholder="/本机/代码仓库/绝对路径" required/>
          <button type="button" className="button secondary" onClick={()=>void browse(workspacePath)}>浏览文件夹…</button>
        </div>
        <small>该路径仅由运行在本机的 Go 服务验证和访问。</small>
      </div>

      {browserOpen&&<section className="directory-browser" aria-label="文件夹浏览器">
        <header>
          <div>
            <span>当前目录</span>
            <code title={listing?.path}>{listing?.path??'正在加载…'}</code>
          </div>
          <button type="button" className="button primary small" disabled={!listing||browseLoading} onClick={chooseCurrentDirectory}>选择当前文件夹</button>
        </header>
        <div className="directory-toolbar">
          <button type="button" className="button ghost small" disabled={!listing?.parentPath||browseLoading} onClick={()=>void browse(listing?.parentPath)}>↑ 返回上级</button>
          <button type="button" className="text-button" onClick={()=>setBrowserOpen(false)}>关闭浏览器</button>
        </div>
        {browseError&&<div className="form-error">打开目录失败：{browseError}</div>}
        <div className="directory-list">
          {browseLoading?<div className="directory-empty">正在加载目录…</div>:listing?.directories.length?listing.directories.map(directory=><button type="button" key={directory.path} onClick={()=>void browse(directory.path)} title={directory.path}><span>▸</span><strong>{directory.name}</strong><small>{directory.path}</small></button>):<div className="directory-empty">没有可读取的子目录</div>}
        </div>
      </section>}

      <label>
        <span>项目说明</span>
        <textarea value={description} onChange={e=>setDescription(e.target.value)} rows={3} placeholder="你正在开发什么？"/>
      </label>
      {mutation.error&&<div className="form-error">创建失败：{mutation.error.message}</div>}
      <footer className="modal-actions">
        <button type="button" className="button ghost" onClick={onClose}>取消</button>
        <button className="button primary" disabled={mutation.isPending}>{mutation.isPending?'正在创建…':'创建项目'}</button>
      </footer>
    </form>
  </Modal>
}
