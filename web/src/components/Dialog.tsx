import { useEffect, useId, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";

interface Props {
  open: boolean;
  title: string;
  description?: ReactNode;
  onClose: () => void;
  children?: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  // Blocks Escape and backdrop dismissal while a request is in flight.
  busy?: boolean;
}

const FOCUSABLE = 'a[href], button, input:not([type="hidden"]), select, textarea, summary, [tabindex], [contenteditable="true"]';

function isRadio(el: Element | null): el is HTMLInputElement {
  return el instanceof HTMLInputElement && el.type === "radio" && el.name !== "";
}

function sameStop(a: Element, b: Element | null): boolean {
  return a === b || (isRadio(a) && isRadio(b) && a.name === b.name && a.form === b.form);
}

// Tab stops in order. :disabled also matches controls inside a disabled fieldset, and a radio
// group is one stop: its checked radio, or its first when none is checked.
function tabStops(root: HTMLElement): HTMLElement[] {
  const all = Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
    (el) => el.tabIndex >= 0 && !el.matches(":disabled") && !el.closest("[hidden], [inert]") && (el.checkVisibility?.() ?? true),
  );
  return all.filter((el) => {
    if (!isRadio(el)) return true;
    const group = all.filter((r) => sameStop(r, el)) as HTMLInputElement[];
    return el === (group.find((r) => r.checked) ?? group[0]);
  });
}

// Makes the page behind the dialog inert, leaving live regions such as toasts announced.
function inertOutside(keep: Element): () => void {
  const changed: Element[] = [];
  const walk = (parent: Element) => {
    for (const el of Array.from(parent.children)) {
      if (el === keep || el.hasAttribute("inert") || el.matches("[aria-live]")) continue;
      if (el.contains(keep) || el.querySelector("[aria-live]")) {
        walk(el);
      } else {
        el.setAttribute("inert", "");
        changed.push(el);
      }
    }
  };
  walk(document.body);
  return () => changed.forEach((el) => el.removeAttribute("inert"));
}

export function Dialog({ open, title, description, onClose, children, footer, wide, busy }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const descId = useId();
  const closeRef = useRef(onClose);
  const busyRef = useRef(busy);
  useEffect(() => {
    closeRef.current = onClose;
    busyRef.current = busy;
  });

  useEffect(() => {
    const node = ref.current;
    if (!open || !node) return;
    const previous = document.activeElement as HTMLElement | null;
    const stops = tabStops(node);
    const auto = node.querySelector<HTMLElement>("[data-autofocus]");
    (auto && stops.includes(auto) ? auto : (stops[0] ?? node)).focus();
    const restoreBackground = inertOutside(node.parentElement ?? node);

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busyRef.current) {
        e.stopPropagation();
        closeRef.current();
      }
      if (e.key !== "Tab") return;
      // Recomputed on every Tab: controls disable and re-enable while a request is pending.
      const items = tabStops(node);
      const active = document.activeElement;
      const at = items.findIndex((el) => sameStop(el, active));
      let next: HTMLElement | undefined;
      if (items.length === 0) {
        next = node;
      } else if (at === -1) {
        // Focus is on the dialog itself, on a control that was just disabled, or outside:
        // move to the nearest stop in the direction of travel, wrapping at the ends.
        const from = active ?? document.body;
        next = e.shiftKey
          ? ([...items].reverse().find((el) => from.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_PRECEDING) ?? items[items.length - 1])
          : (items.find((el) => from.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING) ?? items[0]);
      } else if (e.shiftKey && at === 0) {
        next = items[items.length - 1];
      } else if (!e.shiftKey && at === items.length - 1) {
        next = items[0];
      }
      if (next) {
        e.preventDefault();
        next.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      restoreBackground();
      previous?.focus?.();
    };
  }, [open]);

  if (!open) return null;
  return createPortal(
    <div
      className="dialog-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && !busy) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descId : undefined}
        className={wide ? "dialog dialog-wide" : "dialog"}
        tabIndex={-1}
      >
        <div className="dialog-head">
          <h2 id={titleId}>{title}</h2>
          {description && <p id={descId}>{description}</p>}
        </div>
        {children && <div className="dialog-body">{children}</div>}
        {footer && <div className="dialog-foot">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}
