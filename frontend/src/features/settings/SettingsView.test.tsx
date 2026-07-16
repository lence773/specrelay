// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const apiMocks=vi.hoisted(()=>({
  mcpConnection:vi.fn(),
  diagnoseMcp:vi.fn(),
  rotateMcpToken:vi.fn(),
}))

vi.mock('../../api/client',()=>({
  api:{
    mcpConnection:apiMocks.mcpConnection,
    diagnoseMcp:apiMocks.diagnoseMcp,
    rotateMcpToken:apiMocks.rotateMcpToken,
  },
}))

import { SettingsView } from './SettingsView'

const mcpConnection={
  endpointPath:'/mcp',
  transport:'streamable-http' as const,
  authentication:{scheme:'bearer' as const,description:'使用独立 Bearer Token 鉴权。'},
  token:{state:'configured' as const},
  tools:[
    {name:'list_projects',description:'列出可访问的项目。'},
    {name:'get_plan',description:'读取指定计划。'},
  ],
  serviceName:'SpecRelay MCP',
  serviceVersion:'1.0.0',
  protocolVersion:'2025-03-26',
}

const originalClipboard=navigator.clipboard
const browserMcpAddress=new URL('/mcp',window.location.origin).toString()

function renderSettings(){
  const client=new QueryClient({defaultOptions:{queries:{retry:false}}})
  return render(<QueryClientProvider client={client}><SettingsView/></QueryClientProvider>)
}

function setClipboard(writeText?:ReturnType<typeof vi.fn>){
  Object.defineProperty(navigator,'clipboard',{configurable:true,value:writeText?{writeText}:undefined})
}

beforeEach(()=>{
  apiMocks.mcpConnection.mockResolvedValue(mcpConnection)
  apiMocks.diagnoseMcp.mockResolvedValue({success:true,checkedAt:'2026-07-16T09:15:00Z'})
  apiMocks.rotateMcpToken.mockResolvedValue({token:'mcp-new-token'})
})

afterEach(()=>{
  cleanup()
  vi.restoreAllMocks()
  apiMocks.mcpConnection.mockReset()
  apiMocks.diagnoseMcp.mockReset()
  apiMocks.rotateMcpToken.mockReset()
  Object.defineProperty(navigator,'clipboard',{configurable:true,value:originalClipboard})
  delete (window as Window&{__TAURI__?:unknown}).__TAURI__
})

describe('SettingsView MCP connection',()=>{
  it('展示 MCP 地址、非敏感 Token 状态和服务实际暴露的工具',async()=>{
    renderSettings()

    expect(await screen.findByText(browserMcpAddress)).toBeInTheDocument()
    expect(screen.getByText('Streamable HTTP')).toBeInTheDocument()
    expect(screen.getByText('Bearer Token')).toBeInTheDocument()
    expect(screen.getByText('已配置（不回显）')).toBeInTheDocument()
    expect(screen.getByText('list_projects')).toBeInTheDocument()
    expect(screen.getByText('列出可访问的项目。')).toBeInTheDocument()
    expect(screen.queryByText('mcp-current-secret')).not.toBeInTheDocument()
  })

  it('复制完整连接地址，并在未轮换时只复制 Token 占位符配置',async()=>{
    const writeText=vi.fn().mockResolvedValue(undefined)
    setClipboard(writeText)
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'复制连接地址'}))
    await waitFor(()=>expect(writeText).toHaveBeenLastCalledWith(browserMcpAddress))
    fireEvent.click(screen.getByRole('button',{name:'复制客户端配置'}))
    await waitFor(()=>expect(writeText).toHaveBeenCalledTimes(2))

    const configuration=writeText.mock.calls[1][0] as string
    expect(configuration).toContain(browserMcpAddress)
    expect(configuration).toContain('Bearer <在此填写 MCP Bearer Token>')
    expect(configuration).not.toContain('mcp-current-secret')
    expect(screen.getByRole('status')).toHaveTextContent('已复制 MCP 客户端配置。')
  })

  it('在剪贴板不可用时给出不含 Token 的复制失败反馈',async()=>{
    setClipboard()
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'复制连接地址'}))

    expect(await screen.findByRole('status')).toHaveTextContent('无法复制 MCP 服务地址。请手动复制地址。')
    expect(screen.queryByText('mcp-current-secret')).not.toBeInTheDocument()
  })

  it('发起诊断时显示加载状态，并展示本次成功或失败结果',async()=>{
    let resolveDiagnostic!:(value:{success:boolean;checkedAt:string;failure?:string})=>void
    apiMocks.diagnoseMcp.mockImplementationOnce(()=>new Promise(resolve=>{resolveDiagnostic=resolve}))
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'检测连接'}))
    await waitFor(()=>expect(screen.getByRole('button',{name:'正在检测…'})).toBeDisabled())
    resolveDiagnostic({success:true,checkedAt:'2026-07-16T09:15:00Z'})
    expect(await screen.findByText('检测成功')).toBeInTheDocument()

    apiMocks.diagnoseMcp.mockResolvedValueOnce({success:false,checkedAt:'2026-07-16T09:16:00Z',failure:'MCP 服务暂时不可用。'})
    fireEvent.click(screen.getByRole('button',{name:'检测连接'}))
    expect(await screen.findByText('检测失败')).toBeInTheDocument()
    expect(screen.getByText('MCP 服务暂时不可用。')).toBeInTheDocument()
  })

  it('离开并重新进入设置页后不会回显此前轮换的 Token',async()=>{
    const view=renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'轮换 Token'}))
    fireEvent.click(screen.getByRole('button',{name:'确认轮换并生成新 Token'}))
    expect(await screen.findByText('mcp-new-token')).toBeInTheDocument()

    view.unmount()
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})
    expect(screen.queryByText('mcp-new-token')).not.toBeInTheDocument()
  })

  it('轮换前可取消，成功后仅在当前结果界面显示新 Token',async()=>{
    const writeText=vi.fn().mockResolvedValue(undefined)
    setClipboard(writeText)
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'轮换 Token'}))
    expect(screen.getByRole('dialog',{name:'确认轮换 MCP Token'})).toHaveTextContent('旧 MCP Token 会立即失效')
    fireEvent.click(screen.getByRole('button',{name:'取消'}))
    expect(apiMocks.rotateMcpToken).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button',{name:'轮换 Token'}))
    fireEvent.click(screen.getByRole('button',{name:'确认轮换并生成新 Token'}))
    expect(await screen.findByText('mcp-new-token')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button',{name:'复制新 Token'}))
    await waitFor(()=>expect(writeText).toHaveBeenLastCalledWith('mcp-new-token'))
    fireEvent.click(screen.getByRole('button',{name:'完成并关闭'}))
    expect(screen.queryByText('mcp-new-token')).not.toBeInTheDocument()
  })
})

describe('SettingsView desktop database connection',()=>{
  it('shows the desktop-only message in browser mode',async()=>{
    renderSettings()
    expect(screen.getByText('PostgreSQL 数据库')).toBeInTheDocument()
    expect(screen.getByText('仅桌面端可用')).toBeInTheDocument()
    expect(screen.queryByRole('button',{name:'修改连接'})).not.toBeInTheDocument()
    await screen.findByRole('button',{name:'检测连接'})
  })

  it('opens the secure database reconfiguration flow from the desktop app',async()=>{
    const invoke=vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(window,'__TAURI__',{value:{core:{invoke}},configurable:true})
    const nativeConfirm=vi.spyOn(window,'confirm')
    renderSettings()
    await screen.findByRole('button',{name:'检测连接'})

    fireEvent.click(screen.getByRole('button',{name:'修改连接'}))
    expect(screen.getByRole('dialog',{name:'准备修改数据库连接'})).toBeInTheDocument()
    expect(screen.getByText('不会触碰')).toBeInTheDocument()
    expect(nativeConfirm).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button',{name:'继续修改连接'}))
    await waitFor(()=>expect(invoke).toHaveBeenCalledWith('open_database_configuration'))
  })
})
