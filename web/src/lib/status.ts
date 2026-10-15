import type { StatusCounts } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { EnforcementStatus } from "@harness-control-plane/gen/harnessctl/v1/types_pb";

export type StatusTone = "success" | "progress" | "danger" | "caution" | "neutral" | "muted";

export interface StatusMeta {
  label: string;
  tone: StatusTone;
  // A glyph pairs with the text so the state never rests on colour alone.
  glyph: string;
  description: string;
}

const META: Record<EnforcementStatus, StatusMeta> = {
  [EnforcementStatus.UNSPECIFIED]: { label: "Unknown", tone: "muted", glyph: "?", description: "The server returned no status." },
  [EnforcementStatus.INSTALLED]: { label: "Installed", tone: "success", glyph: "✓", description: "The latest report carries the enforced content." },
  [EnforcementStatus.PENDING]: { label: "Pending", tone: "progress", glyph: "…", description: "Targeted and reachable, not yet converged." },
  [EnforcementStatus.FAILED]: { label: "Failed", tone: "danger", glyph: "✕", description: "The agent reported an apply failure for the current content." },
  [EnforcementStatus.EXCLUDED]: { label: "Excluded", tone: "caution", glyph: "!", description: "No agent has reported for this person, or none knows this repository." },
  [EnforcementStatus.REMOVAL_PENDING]: { label: "Removal pending", tone: "progress", glyph: "…", description: "No longer enforced, but the file is still reported." },
  [EnforcementStatus.REMOVED]: { label: "Removed", tone: "neutral", glyph: "–", description: "No longer enforced and the file is gone." },
  [EnforcementStatus.EXPIRED]: { label: "Expired", tone: "muted", glyph: "◷", description: "The enforcement window closed; agents leave the file as it is." },
};

export function statusMeta(s: EnforcementStatus): StatusMeta {
  return META[s] ?? META[EnforcementStatus.UNSPECIFIED];
}

// Display order: what needs attention first.
export const STATUS_ORDER: EnforcementStatus[] = [
  EnforcementStatus.FAILED,
  EnforcementStatus.EXCLUDED,
  EnforcementStatus.PENDING,
  EnforcementStatus.REMOVAL_PENDING,
  EnforcementStatus.INSTALLED,
  EnforcementStatus.REMOVED,
  EnforcementStatus.EXPIRED,
];

export function countStatuses(statuses: { status: EnforcementStatus }[]): Map<EnforcementStatus, number> {
  const m = new Map<EnforcementStatus, number>();
  for (const s of statuses) m.set(s.status, (m.get(s.status) ?? 0) + 1);
  return m;
}

// countsFromSummary reads the server-computed tallies on an artifact summary.
export function countsFromSummary(c: StatusCounts | undefined): Map<EnforcementStatus, number> {
  return new Map<EnforcementStatus, number>([
    [EnforcementStatus.INSTALLED, c?.installed ?? 0],
    [EnforcementStatus.PENDING, c?.pending ?? 0],
    [EnforcementStatus.FAILED, c?.failed ?? 0],
    [EnforcementStatus.EXCLUDED, c?.excluded ?? 0],
    [EnforcementStatus.REMOVAL_PENDING, c?.removalPending ?? 0],
    [EnforcementStatus.REMOVED, c?.removed ?? 0],
    [EnforcementStatus.EXPIRED, c?.expired ?? 0],
  ]);
}
