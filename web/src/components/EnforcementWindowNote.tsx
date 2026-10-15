import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { daysUntil, formatDate, isPast } from "../lib/format";

// The window length is a server setting, so the copy only ever quotes a date the server returned.
export function EnforcementWindowNote({ enforceUntil }: { enforceUntil?: Timestamp }) {
  let text: string;
  if (!enforceUntil) {
    text = "Enforcement runs for a fixed window from each save, set by the server. Saving again restarts it. The end date appears after the first save.";
  } else if (isPast(enforceUntil)) {
    text = `The enforcement window closed on ${formatDate(enforceUntil)}. Agents leave the files as they are. Saving re-enforces it for a fresh window.`;
  } else {
    const days = Math.max(1, daysUntil(enforceUntil));
    text = `Enforced until ${formatDate(enforceUntil)} (about ${days} day${days === 1 ? "" : "s"}). Saving or changing targets restarts the window.`;
  }
  return <p className="small muted">{text}</p>;
}
