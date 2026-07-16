import { useEffect, useId, useRef, useState } from "react";

export type ThemedSelectOption = {
  value: string;
  label: string;
  disabled?: boolean;
};

export function ThemedSelect({
  ariaLabel,
  value,
  options,
  onChange,
  disabled = false,
  className = "",
}: {
  ariaLabel: string;
  value: string;
  options: ThemedSelectOption[];
  onChange: (value: string) => void;
  disabled?: boolean;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const listboxId = useId();
  const selectedIndex = Math.max(
    0,
    options.findIndex((option) => option.value === value),
  );
  const selected = options[selectedIndex];

  const focusOption = (index: number) => {
    rootRef.current
      ?.querySelector<HTMLButtonElement>(`[data-select-option-index="${index}"]`)
      ?.focus();
  };
  const close = (restoreFocus = false) => {
    setOpen(false);
    if (restoreFocus) requestAnimationFrame(() => triggerRef.current?.focus());
  };
  const openAt = (index: number) => {
    setOpen(true);
    requestAnimationFrame(() => focusOption(index));
  };
  const enabledIndex = (start: number, direction: 1 | -1) => {
    for (let step = 0; step < options.length; step += 1) {
      const index = (start + direction * step + options.length) % options.length;
      if (!options[index]?.disabled) return index;
    }
    return selectedIndex;
  };

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) close();
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  useEffect(() => {
    if (disabled) setOpen(false);
  }, [disabled]);

  return (
    <div
      ref={rootRef}
      className={`themed-select${open ? " open" : ""}${className ? ` ${className}` : ""}`}
    >
      <button
        ref={triggerRef}
        type="button"
        className="themed-select-trigger"
        role="combobox"
        aria-label={ariaLabel}
        aria-controls={listboxId}
        aria-expanded={open}
        aria-haspopup="listbox"
        disabled={disabled}
        onClick={() => setOpen((current) => !current)}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            close();
            return;
          }
          if (event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            if (open) close();
            else openAt(enabledIndex(selectedIndex, 1));
            return;
          }
          if (event.key === "ArrowDown") {
            event.preventDefault();
            openAt(enabledIndex(selectedIndex + 1, 1));
            return;
          }
          if (event.key === "ArrowUp") {
            event.preventDefault();
            openAt(enabledIndex(selectedIndex - 1, -1));
          }
        }}
      >
        <span>{selected?.label ?? "请选择"}</span>
        <i className="themed-select-indicator" aria-hidden="true">
          <svg viewBox="0 0 20 20" fill="none">
            <path d="m5.5 7.5 4.5 4.5 4.5-4.5" />
          </svg>
        </i>
      </button>
      {open && (
        <div id={listboxId} className="themed-select-menu" role="listbox" aria-label={ariaLabel}>
          {options.map((option, index) => (
            <button
              type="button"
              role="option"
              key={option.value}
              data-select-option-index={index}
              aria-selected={option.value === value}
              disabled={option.disabled}
              className={option.value === value ? "selected" : ""}
              onClick={() => {
                onChange(option.value);
                close(true);
              }}
              onKeyDown={(event) => {
                if (event.key === "Escape") {
                  event.preventDefault();
                  close(true);
                  return;
                }
                if (event.key === "ArrowDown") {
                  event.preventDefault();
                  focusOption(enabledIndex(index + 1, 1));
                  return;
                }
                if (event.key === "ArrowUp") {
                  event.preventDefault();
                  focusOption(enabledIndex(index - 1, -1));
                  return;
                }
                if (event.key === "Home") {
                  event.preventDefault();
                  focusOption(enabledIndex(0, 1));
                  return;
                }
                if (event.key === "End") {
                  event.preventDefault();
                  focusOption(enabledIndex(options.length - 1, -1));
                }
              }}
            >
              <span>{option.label}</span>
              {option.value === value && <b aria-hidden="true">✓</b>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
