import { useMemo, useState } from "react";
import { EnforcementStatus } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { useEnforcementStatus } from "../api/queries";
import { formatDateTime } from "../lib/format";
import { scopeLabel } from "../lib/labels";
import { useNames } from "../lib/names";
import { countStatuses, STATUS_ORDER, statusMeta } from "../lib/status";
import { Empty, ErrorState, Loading } from "./States";
import { StatusBadge } from "./StatusBadge";
import { StatusCounts } from "./StatusCounts";

export function StatusTable({ artifactId }: { artifactId: string }) {
  const q = useEnforcementStatus(artifactId);
  const names = useNames();
  const [filter, setFilter] = useState<EnforcementStatus | "all">("all");

  const rows = useMemo(() => {
    const all = [...(q.data?.statuses ?? [])];
    all.sort(
      (a, b) =>
        STATUS_ORDER.indexOf(a.status) - STATUS_ORDER.indexOf(b.status) ||
        names.person(a.personId).localeCompare(names.person(b.personId)),
    );
    return filter === "all" ? all : all.filter((r) => r.status === filter);
  }, [q.data, filter, names]);
  const counts = countStatuses(q.data?.statuses ?? []);
  // A single-placement artifact would repeat the same value on every row.
  const multiPlacement = new Set((q.data?.statuses ?? []).map((r) => `${r.scope}|${r.repository}`)).size > 1;

  return (
    <section className="card" aria-labelledby="status-h">
      <div className="card-head">
        <div className="stack" style={{ gap: 2 }}>
          <h2 id="status-h">Per-person status</h2>
          <p>
            From each person's most recent agent report. A person on several machines shows whichever reported last.
            {!multiPlacement && q.data && q.data.statuses[0] && <> Placement: {scopeLabel(q.data.statuses[0].scope, q.data.statuses[0].repository)}.</>}
          </p>
        </div>
        <div className="spacer" />
        <label className="row small" style={{ gap: 6 }}>
          <span className="muted">Show</span>
          <select className="select" style={{ width: "auto" }} value={filter} onChange={(e) => setFilter(e.target.value === "all" ? "all" : (Number(e.target.value) as EnforcementStatus))}>
            <option value="all">All statuses ({q.data?.statuses.length ?? 0})</option>
            {STATUS_ORDER.filter((s) => counts.get(s)).map((s) => (
              <option key={s} value={s}>
                {statusMeta(s).label} ({counts.get(s)})
              </option>
            ))}
          </select>
        </label>
      </div>
      {q.data && q.data.statuses.length > 0 && (
        <div className="card-body" style={{ paddingBottom: 0 }}>
          <StatusCounts counts={counts} />
        </div>
      )}
      {q.isLoading && <Loading />}
      {q.error && <ErrorState error={q.error} onRetry={() => void q.refetch()} />}
      {q.data && q.data.statuses.length === 0 && (
        <Empty title="Nobody is targeted">
          <p>Add a placement to start enforcing this artifact.</p>
        </Empty>
      )}
      {rows.length > 0 && (
        <div className="table-wrap" style={{ marginTop: 12 }}>
          <table className="table">
            <thead>
              <tr>
                <th scope="col">Person</th>
                {multiPlacement && <th scope="col">Placement</th>}
                <th scope="col">Status</th>
                <th scope="col" style={{ minWidth: 180 }}>Detail</th>
                <th scope="col" className="num">Reported</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={`${r.personId}|${r.scope}|${r.repository}`}>
                  <td className="nowrap">{names.person(r.personId)}</td>
                  {multiPlacement && <td className="small">{scopeLabel(r.scope, r.repository)}</td>}
                  <td>
                    <StatusBadge status={r.status} />
                  </td>
                  <td className="small" style={{ color: r.status === EnforcementStatus.FAILED ? "var(--danger-fg)" : "var(--fg-2)", wordBreak: "break-word" }}>
                    {r.detail || <span className="muted">—</span>}
                  </td>
                  <td className="num small">{formatDateTime(r.observedAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
