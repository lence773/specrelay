import { describe, expect, it } from 'vitest'
import { parseTerminalLines } from './terminal'

const codex=(event:unknown)=>JSON.stringify(event)

describe('parseTerminalLines',()=>{
  it('renders Codex todos and command execution as readable terminal lines',()=>{
    const lines=parseTerminalLines([
      codex({type:'item.started',item:{type:'todo_list',items:[{text:'检查项目结构',completed:false},{text:'生成计划',completed:true}]}}),
      codex({type:'item.completed',item:{type:'command_execution',command:"/bin/bash -lc 'pwd'",aggregated_output:'/workspace',exit_code:0}}),
    ].join('\n'),'codex')
    expect(lines).toEqual(expect.arrayContaining([
      expect.objectContaining({kind:'todo',marker:'○',text:'检查项目结构'}),
      expect.objectContaining({kind:'todo',marker:'✓',text:'生成计划'}),
      expect.objectContaining({kind:'command',text:'pwd'}),
      expect.objectContaining({kind:'success',text:'命令执行完成（退出码 0，已省略具体输出）。'}),
    ]))
  })

  it('shows a started command without adding a redundant running message',()=>{
    const lines=parseTerminalLines(codex({type:'item.started',item:{type:'command_execution',command:"/bin/bash -lc 'pwd'",aggregated_output:'',exit_code:null}}),'codex')
    expect(lines).toEqual([{kind:'command',marker:'$',text:'pwd'}])
  })

  it('summarizes a generated plan instead of displaying its large JSON payload',()=>{
    const plan={title:'增加 CLI 提供方选择',summary:'支持在各个入口选择 Codex 或 Claude。',tasks:[{title:'前端'},{title:'后端'}]}
    const lines=parseTerminalLines(codex({type:'item.completed',item:{type:'agent_message',text:JSON.stringify(plan)}}),'codex')
    expect(lines).toEqual(expect.arrayContaining([
      expect.objectContaining({kind:'assistant',text:'计划已生成：增加 CLI 提供方选择'}),
      expect.objectContaining({kind:'assistant',text:'支持在各个入口选择 Codex 或 Claude。'}),
      expect.objectContaining({kind:'success',text:'已整理 2 个实施项。'}),
    ]))
    expect(lines.some(line=>line.text.includes('"tasks"'))).toBe(false)
  })

  it('does not expose unknown Codex JSON events as unreadable raw JSON',()=>{
    const lines=parseTerminalLines(codex({type:'internal.noise',payload:{large:true}}),'codex')
    expect(lines).toEqual([{kind:'system',marker:'…',text:'正在接收 Codex CLI 的运行事件…'}])
  })

  it('keeps non-Codex output concise instead of exposing command details',()=>{
    expect(parseTerminalLines('\u001b[32mchecking\u001b[0m\ncompleted','validation')).toEqual([
      {kind:'system',marker:'›',text:'已接收 2 条终端输出；为保持运行记录简洁，未展示具体内容。'},
    ])
  })
})
