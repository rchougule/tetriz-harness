import { timestampDate, type Timestamp } from "@bufbuild/protobuf/wkt";

const dateFmt = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" });
const dateTimeFmt = new Intl.DateTimeFormat(undefined, {
  year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
});

export function tsDate(ts: Timestamp | undefined): Date | null {
  return ts ? timestampDate(ts) : null;
}

export function formatDate(ts: Timestamp | undefined): string {
  const d = tsDate(ts);
  return d ? dateFmt.format(d) : "—";
}

export function formatDateTime(ts: Timestamp | undefined): string {
  const d = tsDate(ts);
  return d ? dateTimeFmt.format(d) : "—";
}

export function isPast(ts: Timestamp | undefined, now = Date.now()): boolean {
  const d = tsDate(ts);
  return d !== null && d.getTime() < now;
}

export function daysUntil(ts: Timestamp, now = Date.now()): number {
  return Math.ceil((timestampDate(ts).getTime() - now) / 86_400_000);
}

const relFmt = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

// Coarse on purpose: last use is itself recorded at most once a minute.
export function timeAgo(ts: Timestamp | undefined, now = Date.now()): string {
  const d = tsDate(ts);
  if (!d) return "—";
  const mins = Math.round((d.getTime() - now) / 60_000);
  if (mins > -1) return "just now";
  if (mins > -60) return relFmt.format(mins, "minute");
  const hours = Math.round(mins / 60);
  if (hours > -24) return relFmt.format(hours, "hour");
  return relFmt.format(Math.round(hours / 24), "day");
}
