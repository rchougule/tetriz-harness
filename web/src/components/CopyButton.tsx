import { useEffect, useState } from "react";
import { copyText } from "../lib/clipboard";

interface Props {
  text: string;
  label?: string;
  // Names what is copied for screen readers when several copy buttons share a view.
  subject?: string;
  className?: string;
}

export function CopyButton({ text, label = "Copy", subject, className = "btn btn-sm" }: Props) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  useEffect(() => {
    if (state === "idle") return;
    const t = setTimeout(() => setState("idle"), 1800);
    return () => clearTimeout(t);
  }, [state]);
  return (
    <button
      type="button"
      className={className}
      onClick={async () => setState((await copyText(text)) ? "copied" : "failed")}
      aria-label={subject ? `${label} ${subject}` : undefined}
    >
      {state === "copied" ? "Copied" : state === "failed" ? "Copy failed — select and copy manually" : label}
      <span className="visually-hidden" aria-live="polite">
        {state === "copied" ? "Copied to clipboard" : ""}
      </span>
    </button>
  );
}
