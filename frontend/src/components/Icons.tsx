import type { SVGProps } from 'react'
const Icon=({children,...props}:SVGProps<SVGSVGElement>)=><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" {...props}>{children}</svg>
export const Logo=()=> <svg viewBox="0 0 42 42" aria-hidden="true"><path d="M8 28.5 21 6l13 22.5-13 7.5L8 28.5Z" fill="currentColor" opacity=".16"/><path d="m8 28.5 13-7.2 13 7.2M21 6v15.3M8 28.5l13 7.5 13-7.5" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinejoin="round"/></svg>
export const Plus=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M12 5v14M5 12h14"/></Icon>
export const Dashboard=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/></Icon>
export const Inbox=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M4 4h16v13H4z"/><path d="M4 13h4l2 3h4l2-3h4"/></Icon>
export const PlanIcon=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M6 3h12v18H6z"/><path d="M9 8h6M9 12h6M9 16h4"/></Icon>
export const Activity=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M3 12h4l2.5-7 5 14 2.5-7h4"/></Icon>
export const SettingsIcon=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .34 1.88l.06.06-2.83 2.83-.06-.06A1.7 1.7 0 0 0 15 19.4a1.7 1.7 0 0 0-1 .6 1.7 1.7 0 0 0-.4 1.1V21h-4v-.1A1.7 1.7 0 0 0 8.6 19.4a1.7 1.7 0 0 0-1.88.34l-.06.06-2.83-2.83.06-.06A1.7 1.7 0 0 0 4.2 15a1.7 1.7 0 0 0-.6-1 1.7 1.7 0 0 0-1.1-.4H2v-4h.5A1.7 1.7 0 0 0 4.2 8.6a1.7 1.7 0 0 0-.34-1.88l-.06-.06 2.83-2.83.06.06A1.7 1.7 0 0 0 8.6 4.2a1.7 1.7 0 0 0 1-.6 1.7 1.7 0 0 0 .4-1.1V2h4v.5a1.7 1.7 0 0 0 1 1.7 1.7 1.7 0 0 0 1.88-.34l.06-.06 2.83 2.83-.06.06A1.7 1.7 0 0 0 19.4 8.6a1.7 1.7 0 0 0 .6 1 1.7 1.7 0 0 0 1.1.4h.5v4h-.5a1.7 1.7 0 0 0-1.7 1Z"/></Icon>
export const Folder=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M3 6h7l2 2h9v11H3z"/></Icon>
export const Chevron=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="m9 18 6-6-6-6"/></Icon>
export const Play=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="m8 5 11 7-11 7z"/></Icon>
export const Stop=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><rect x="6" y="6" width="12" height="12" rx="1"/></Icon>
export const Upload=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="M12 16V4m0 0L7 9m5-5 5 5M4 15v5h16v-5"/></Icon>
export const Check=(p:SVGProps<SVGSVGElement>)=><Icon {...p}><path d="m5 12 4 4L19 6"/></Icon>
