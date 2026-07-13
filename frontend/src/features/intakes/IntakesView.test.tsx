// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useState } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Project } from '../../api/types'

const {discussRequirement}=vi.hoisted(()=>({discussRequirement:vi.fn()}))
vi.mock('../../api/client',()=>({api:{discussRequirement}}))

import { IntakesView } from './IntakesView'

const project={
  id:'11111111-1111-4111-8111-111111111111',
  name:'缓存测试项目',
  description:'',
  workspacePath:'/tmp/cache-test',
  automationEnabled:false,
  createdAt:'2026-07-14T00:00:00Z',
  updatedAt:'2026-07-14T00:00:00Z',
  version:1,
} as Project

function RequirementTabHost(){
  const[visible,setVisible]=useState(true)
  return <QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}>
    <button onClick={()=>setVisible(false)}>切换到计划</button>
    <button onClick={()=>setVisible(true)}>返回需求</button>
    <div hidden={!visible}><IntakesView project={project} intakes={[]}/></div>
  </QueryClientProvider>
}

describe('IntakesView tab cache',()=>{
  beforeEach(()=>{discussRequirement.mockReset()})

  it('keeps unfinished form content and CLI discussion results when the tab is hidden',async()=>{
    discussRequirement.mockResolvedValue({provider:'codex',reply:'建议补充验收标准。',ready:false,title:'缓存后的标题',body:'CLI 整理后的详细说明'})
    render(<RequirementTabHost/>)

    fireEvent.click(screen.getByRole('button',{name:'创建需求'}))
    fireEvent.change(screen.getByPlaceholderText('希望做出什么改变？'),{target:{value:'初始标题'}})
    fireEvent.change(screen.getByPlaceholderText('请描述背景、约束条件和期望结果…'),{target:{value:'初始说明'}})
    fireEvent.click(screen.getByRole('button',{name:'开始讨论'}))
    fireEvent.change(screen.getByPlaceholderText('描述你的想法，或回答 CLI 提出的澄清问题…'),{target:{value:'请帮我补充细节'}})
    fireEvent.click(screen.getByRole('button',{name:'发送给 CLI'}))

    await waitFor(()=>expect(screen.getByText('建议补充验收标准。')).toBeInTheDocument())
    expect(screen.getByPlaceholderText('希望做出什么改变？')).toHaveValue('缓存后的标题')
    expect(screen.getByPlaceholderText('请描述背景、约束条件和期望结果…')).toHaveValue('CLI 整理后的详细说明')

    fireEvent.click(screen.getByRole('button',{name:'切换到计划'}))
    fireEvent.click(screen.getByRole('button',{name:'返回需求'}))

    expect(screen.getByText('请帮我补充细节')).toBeInTheDocument()
    expect(screen.getByText('建议补充验收标准。')).toBeInTheDocument()
    expect(screen.getByPlaceholderText('希望做出什么改变？')).toHaveValue('缓存后的标题')
    expect(screen.getByPlaceholderText('请描述背景、约束条件和期望结果…')).toHaveValue('CLI 整理后的详细说明')
  })
})
