import type { ReactNode } from 'react'

export function Modal({title,children,onClose,wide=false}: {title:string;children:ReactNode;onClose:()=>void;wide?:boolean}) {
  return <div className="modal-backdrop" onMouseDown={e=>{if(e.target===e.currentTarget)onClose()}}>
    <section className={`modal${wide?' wide':''}`}>
      <header>
        <div><span className="eyebrow">SpecRelay</span><h2>{title}</h2></div>
        <button type="button" className="icon-button" onClick={onClose} aria-label="关闭">×</button>
      </header>
      {children}
    </section>
  </div>
}
