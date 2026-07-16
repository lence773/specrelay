// @vitest-environment jsdom
import '@testing-library/jest-dom/vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SettingsView } from './SettingsView'

function renderSettings(){
  const client=new QueryClient({defaultOptions:{queries:{retry:false}}})
  return render(<QueryClientProvider client={client}><SettingsView/></QueryClientProvider>)
}

afterEach(()=>{
  cleanup()
  vi.restoreAllMocks()
  delete (window as Window&{__TAURI__?:unknown}).__TAURI__
})

describe('SettingsView desktop database connection',()=>{
  it('shows the desktop-only message in browser mode',()=>{
    renderSettings()
    expect(screen.getByText('PostgreSQL 数据库')).toBeInTheDocument()
    expect(screen.getByText('仅桌面端可用')).toBeInTheDocument()
    expect(screen.queryByRole('button',{name:'修改连接'})).not.toBeInTheDocument()
  })

  it('opens the secure database reconfiguration flow from the desktop app',async()=>{
    const invoke=vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(window,'__TAURI__',{value:{core:{invoke}},configurable:true})
    const nativeConfirm=vi.spyOn(window,'confirm')
    renderSettings()

    fireEvent.click(screen.getByRole('button',{name:'修改连接'}))
    expect(screen.getByRole('dialog',{name:'准备修改数据库连接'})).toBeInTheDocument()
    expect(screen.getByText('不会触碰')).toBeInTheDocument()
    expect(nativeConfirm).not.toHaveBeenCalled()

    fireEvent.click(screen.getByRole('button',{name:'继续修改连接'}))
    await waitFor(()=>expect(invoke).toHaveBeenCalledWith('open_database_configuration'))
  })
})
