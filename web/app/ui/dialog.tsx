"use client";

import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { X } from "lucide-react";
import { IconButton } from "./controls";

/**
 * Centred dialog on desktop, bottom sheet under 640px. A native <dialog>
 * opened with showModal(): the browser makes the page behind it inert and
 * keeps keyboard focus inside. Escape and a backdrop click close it, and
 * focus returns to whatever opened it.
 */
export function Modal({ title, onClose, children, footer }: { title: string; onClose: () => void; children: ReactNode; footer?: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  // Captured at first render: autoFocus inside the dialog moves focus before effects run.
  const [opener] = useState(() => (typeof document !== "undefined" && document.activeElement instanceof HTMLElement ? document.activeElement : null));
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);
  useLayoutEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (typeof dialog.showModal === "function") dialog.showModal();
    else dialog.setAttribute("open", ""); // jsdom
    const onCancel = (event: Event) => {
      event.preventDefault();
      onCloseRef.current();
    };
    dialog.addEventListener("cancel", onCancel);
    return () => {
      dialog.removeEventListener("cancel", onCancel);
      if (dialog.open && typeof dialog.close === "function") dialog.close();
      opener?.focus();
    };
  }, [opener]);
  return (
    <dialog
      ref={ref}
      aria-label={title}
      onClick={(event) => event.target === event.currentTarget && onClose()}
      className="fixed inset-0 z-50 m-0 h-full max-h-none w-full max-w-none items-end justify-center border-0 bg-transparent p-0 backdrop:bg-black/50 open:flex sm:items-center sm:p-4"
    >
      <div className="max-h-[90dvh] w-full overflow-y-auto rounded-t-[10px] border border-border bg-surface pb-[env(safe-area-inset-bottom)] shadow-[var(--shadow-xl)] sm:max-w-md sm:rounded-[10px] sm:pb-0">
        <div className="flex items-center justify-between border-b border-border-faint py-2 pl-5 pr-2">
          <h2 className="font-mono text-xs font-bold uppercase tracking-[0.2em] text-fg">{title}</h2>
          <IconButton label="Close" onClick={onClose}>
            <X aria-hidden className="h-4 w-4" />
          </IconButton>
        </div>
        <div className="space-y-4 p-5">{children}</div>
        {footer && <div className="flex justify-end gap-2 border-t border-border-faint px-5 py-3">{footer}</div>}
      </div>
    </dialog>
  );
}
