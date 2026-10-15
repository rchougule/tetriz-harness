import type { Audience } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import type { AudienceDraft } from "./validate";

export function audienceToDraft(a: Audience | undefined): AudienceDraft {
  if (!a || a.orgWide) return { mode: "org", personIds: [], teamIds: [] };
  if (a.teamIds.length > 0 && a.personIds.length === 0) return { mode: "teams", personIds: [], teamIds: [...a.teamIds] };
  return { mode: "persons", personIds: [...a.personIds], teamIds: [] };
}

export function draftToAudience(d: AudienceDraft) {
  if (d.mode === "org") return { orgWide: true, personIds: [], teamIds: [] };
  if (d.mode === "teams") return { orgWide: false, personIds: [], teamIds: d.teamIds };
  return { orgWide: false, personIds: d.personIds, teamIds: [] };
}

export interface NameLookup {
  person: (id: string) => string;
  team: (id: string) => string;
}

function names(ids: string[], lookup: (id: string) => string, one: string, many: string): string {
  if (ids.length > 2) return `${ids.length} ${many}`;
  return `${ids.map(lookup).join(", ")}${one ? ` ${ids.length === 1 ? one : many}` : ""}`;
}

export function audienceSummary(a: Audience | undefined, lookup: NameLookup): string {
  if (!a) return "Nobody";
  if (a.orgWide) return "Everyone";
  const parts: string[] = [];
  if (a.teamIds.length) parts.push(names(a.teamIds, lookup.team, "team", "teams"));
  if (a.personIds.length) parts.push(a.personIds.length > 2 ? `${a.personIds.length} people` : a.personIds.map(lookup.person).join(", "));
  return parts.join(" + ") || "Nobody";
}
