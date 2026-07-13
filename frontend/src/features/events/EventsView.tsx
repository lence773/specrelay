import type { EventRecord } from '../../api/types'
import { Activity } from '../../components/Icons'
import { Empty, eventLabel, relative, resourceLabel } from '../../components/Status'

export function EventsView({events}:{events:EventRecord[]}) {
  return <div className="page narrow">
    <section className="hero compact"><div><span className="eyebrow">不可变历史记录</span><h1>事件流</h1><p>实时查看保存在 PostgreSQL 中的资源变更、队列状态和智能体活动。</p></div><span className="live-label"><i/> SSE 已连接</span></section>
    <section className="panel event-panel">{events.length===0?<Empty title="正在等待活动" body="新事件会实时显示，并在重新连接后继续保留。"/>:<div className="event-table"><div className="event-head"><span>事件</span><span>资源</span><span>版本</span><span>时间</span></div>{[...events].reverse().map(event=><div className="event-row" key={event.id}><span><i className="event-icon"><Activity/></i><strong>{eventLabel(event.type)}</strong></span><span><b>{resourceLabel(event.aggregateType)}</b><code>{event.aggregateId.slice(0,8)}</code></span><span>v{event.resourceVersion}</span><span>{relative(event.occurredAt)}</span></div>)}</div>}</section>
  </div>
}
