import type { EnforcementStatus } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { statusMeta } from "../lib/status";

export function StatusBadge({ status }: { status: EnforcementStatus }) {
  const m = statusMeta(status);
  return (
    <span className={`badge tone-${m.tone}`} title={m.description}>
      <span className="badge-glyph" aria-hidden="true">{m.glyph}</span>
      {m.label}
    </span>
  );
}
