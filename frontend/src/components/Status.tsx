const statusLabels:Record<string,string>={
  open:'待处理',
  planning:'规划中',
  planned:'已规划',
  closed:'已关闭',
  plan_failed:'规划失败',
  generating:'生成中',
  ready:'就绪',
  running:'运行中',
  validating:'验证中',
  completed:'已完成',
  blocked:'已阻塞',
  cancelled:'已取消',
  pending:'待执行',
  queued:'排队中',
  leased:'已领取',
  succeeded:'成功',
  failed:'失败',
  retry_wait:'等待重试',
}

const eventLabels:Record<string,string>={
  'agent.output':'智能体输出',
  'intake.created':'需求已创建',
  'intake.updated':'需求已更新',
  'intake.planning':'需求规划中',
  'intake.planned':'需求已规划',
  'intake.closed':'需求已关闭',
  'intake.plan_failed':'需求规划失败',
  'intake.plan_cancelled':'需求规划已取消',
  'plan.generate':'开始生成计划',
  'plan.ready':'计划已就绪',
  'plan.running':'计划运行中',
  'plan.validating':'计划验证中',
  'plan.completed':'计划已完成',
  'plan.blocked':'计划已阻塞',
  'plan.cancelled':'计划已取消',
  'plan.deleted':'计划已删除',
  'task.execute':'开始执行任务',
  'task.queued':'任务已排队',
  'task.retry_wait':'任务等待重试',
  'task.succeeded':'任务执行成功',
  'task.failed':'任务执行失败',
  'task.cancelled':'任务已取消',
  'project.created':'项目已创建',
  'project.updated':'项目已更新',
  'project.automation_started':'自动化已启动',
  'project.automation_stopped':'自动化已停止',
}

const resourceLabels:Record<string,string>={project:'项目',intake:'需求',plan:'计划',task:'任务',job:'作业',attachment:'附件'}

export function Status({value}:{value:string}) {
  return <span className={`status status-${value}`}>{statusLabels[value]??value.replaceAll('_',' ')}</span>
}

export function Empty({title,body,action}:{title:string;body:string;action?:React.ReactNode}) {
  return <div className="empty"><div className="empty-mark">◇</div><h3>{title}</h3><p>{body}</p>{action}</div>
}

export function kindLabel(value:string) {
  return value==='requirement'?'需求':value==='feedback'?'反馈':value
}

export function eventLabel(value:string) {
  return eventLabels[value]??value.replaceAll('.',' · ').replaceAll('_',' ')
}

export function resourceLabel(value:string) {
  return resourceLabels[value]??value
}

export const relative=(value:string)=>{
  const delta=Math.max(0,Date.now()-new Date(value).getTime())
  if(delta<60_000)return '刚刚'
  if(delta<3_600_000)return `${Math.floor(delta/60_000)} 分钟前`
  if(delta<86_400_000)return `${Math.floor(delta/3_600_000)} 小时前`
  if(delta<7*86_400_000)return `${Math.floor(delta/86_400_000)} 天前`
  return new Date(value).toLocaleDateString('zh-CN')
}
