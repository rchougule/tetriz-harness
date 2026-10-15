import type { ReactNode } from "react";
import { errorMessage } from "../api/errors";

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="loading" role="status">
      {label}
    </div>
  );
}

interface ErrorProps {
  error: unknown;
  onRetry?: () => void;
  // Names what failed when the callout sits inside a larger form.
  what?: string;
}

export function ErrorCallout({ error, onRetry, what }: ErrorProps) {
  return (
    <div className="callout callout-danger" role="alert">
      <span className="icon" aria-hidden="true">✕</span>
      <div className="stack" style={{ gap: 8 }}>
        <span>{what ? `${what}: ${errorMessage(error)}` : errorMessage(error)}</span>
        {onRetry && (
          <span>
            <button type="button" className="btn btn-sm" onClick={onRetry}>
              Retry
            </button>
          </span>
        )}
      </div>
    </div>
  );
}

export function ErrorState(props: ErrorProps) {
  return (
    <div className="card-body">
      <ErrorCallout {...props} />
    </div>
  );
}

export function Empty({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {children}
    </div>
  );
}
