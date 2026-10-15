import { useId, type ReactNode } from "react";

export interface FieldControlProps {
  id: string;
  "aria-invalid": boolean;
  "aria-describedby"?: string;
}

interface Props {
  label: string;
  hint?: ReactNode;
  error?: string;
  className?: string;
  children: (props: FieldControlProps) => ReactNode;
}

export function Field({ label, hint, error, className, children }: Props) {
  const id = useId();
  const hintId = hint ? `${id}-hint` : undefined;
  const errId = error ? `${id}-err` : undefined;
  const describedBy = [hintId, errId].filter(Boolean).join(" ") || undefined;
  return (
    <div className={className ? `field ${className}` : "field"}>
      <label htmlFor={id}>{label}</label>
      {children({ id, "aria-invalid": Boolean(error), "aria-describedby": describedBy })}
      {hint && <span id={hintId} className="hint">{hint}</span>}
      {error && <span id={errId} className="error">{error}</span>}
    </div>
  );
}
