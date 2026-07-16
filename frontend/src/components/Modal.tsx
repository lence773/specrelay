import { useEffect, useId, type ReactNode } from "react";

export function Modal({
  title,
  children,
  onClose,
  wide = false,
  className = "",
  closeDisabled = false,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  wide?: boolean;
  className?: string;
  closeDisabled?: boolean;
}) {
  const titleId = useId();
  const requestClose = () => {
    if (!closeDisabled) onClose();
  };
  useEffect(() => {
    const close = (event: KeyboardEvent) => {
      if (event.key === "Escape") requestClose();
    };
    window.addEventListener("keydown", close);
    return () => window.removeEventListener("keydown", close);
  }, [closeDisabled, onClose]);
  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) requestClose();
      }}
    >
      <section
        className={`modal${wide ? " wide" : ""}${className ? ` ${className}` : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
      >
        <header>
          <div>
            <span className="eyebrow">SpecRelay</span>
            <h2 id={titleId}>{title}</h2>
          </div>
          <button
            type="button"
            className="icon-button"
            onClick={requestClose}
            aria-label="关闭"
            disabled={closeDisabled}
          >
            ×
          </button>
        </header>
        {children}
      </section>
    </div>
  );
}
