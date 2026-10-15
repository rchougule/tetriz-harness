import type { ReactNode } from "react";
import { Dialog } from "./Dialog";

interface Props {
  open: boolean;
  title: string;
  description?: ReactNode;
  children?: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  pending?: boolean;
  error?: string | null;
  onConfirm: () => void;
  onCancel: () => void;
}

export function ConfirmDialog({ open, title, description, children, confirmLabel, danger, pending, error, onConfirm, onCancel }: Props) {
  return (
    <Dialog
      open={open}
      title={title}
      description={description}
      onClose={onCancel}
      busy={pending}
      footer={
        <>
          <button type="button" className="btn" onClick={onCancel} disabled={pending} data-autofocus>
            Cancel
          </button>
          <button type="button" className={danger ? "btn btn-danger-solid" : "btn btn-primary"} onClick={onConfirm} disabled={pending}>
            {pending ? "Working…" : confirmLabel}
          </button>
        </>
      }
    >
      {children}
      {error && (
        <p role="alert" className="callout callout-danger" style={{ marginTop: children ? 10 : 0 }}>
          {error}
        </p>
      )}
    </Dialog>
  );
}
