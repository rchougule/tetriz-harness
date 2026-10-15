import type { EnforcementStatus } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { STATUS_ORDER, statusMeta } from "../lib/status";

export function StatusCounts({ counts }: { counts: Map<EnforcementStatus, number> }) {
  const shown = STATUS_ORDER.filter((s) => counts.get(s));
  if (shown.length === 0) return <span className="muted small">No one targeted</span>;
  return (
    <ul className="counts" style={{ listStyle: "none", margin: 0, padding: 0 }} aria-label="Status counts">
      {shown.map((s) => {
        const m = statusMeta(s);
        return (
          <li key={s} className={`count-chip tone-${m.tone}`}>
            <span aria-hidden="true">{m.glyph}</span>
            {counts.get(s)} {m.label.toLowerCase()}
          </li>
        );
      })}
    </ul>
  );
}
