import { useState } from "react";
import { Link } from "react-router";
import type { ArtifactSummary } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { useArtifacts } from "../api/queries";
import { Empty, ErrorState, Loading } from "../components/States";
import { RetractDialog } from "../components/RetractDialog";
import { StatusCounts } from "../components/StatusCounts";
import { ArtifactScope } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { audienceSummary } from "../lib/audience";
import { formatDate, isPast } from "../lib/format";
import { ideLabel, typeLabel } from "../lib/labels";
import { useNames } from "../lib/names";
import { countsFromSummary } from "../lib/status";

export function ArtifactsPage() {
  const [showRetracted, setShowRetracted] = useState(false);
  const artifacts = useArtifacts(showRetracted);
  const list = artifacts.data?.artifacts ?? [];
  const names = useNames();
  const [retracting, setRetracting] = useState<ArtifactSummary | null>(null);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Artifacts</h1>
          <p>Skills, rules, agents, hooks and instruction files your agents install on targeted machines.</p>
        </div>
        <div className="row">
          <label className="row small" style={{ gap: 6 }}>
            <input type="checkbox" checked={showRetracted} onChange={(e) => setShowRetracted(e.target.checked)} />
            Show retracted
          </label>
          <Link to="/artifacts/new" className="btn btn-primary">
            New artifact
          </Link>
        </div>
      </div>

      <section className="card" aria-label="Artifacts">
        {artifacts.isLoading && <Loading />}
        {artifacts.error && <ErrorState error={artifacts.error} onRetry={() => void artifacts.refetch()} />}
        {artifacts.data && list.length === 0 && (
          <Empty title="No artifacts yet">
            <p>Create one and target it to people, teams or the whole org.</p>
          </Empty>
        )}
        {list.length > 0 && (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th scope="col" style={{ minWidth: 160 }}>Artifact</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Targets</th>
                  <th scope="col">Status</th>
                  <th scope="col">Enforced until</th>
                  <th scope="col">
                    <span className="visually-hidden">Actions</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {list.map((a) => {
                  return (
                    <tr key={a.artifactId} className={a.retractedAt ? "row-retracted" : undefined}>
                      <td>
                        <Link to={`/artifacts/${a.artifactId}`} className="cell-title">
                          {a.name || a.slug}
                        </Link>
                        <div className="cell-sub">{a.slug}</div>
                      </td>
                      <td>
                        <span className="tag">{typeLabel(a.type)}</span>
                        <div className="small muted nowrap" style={{ marginTop: 4 }}>
                          {ideLabel(a.ideType)}
                        </div>
                      </td>
                      <td>
                        {a.placements.length === 0 ? (
                          <span className="muted small">Not targeted</span>
                        ) : (
                          a.placements.map((p) => (
                            <div key={`${p.scope}:${p.repository}`} className="small" style={{ marginBottom: 4 }}>
                              <div>{audienceSummary(p.audience, names)}</div>
                              <div className="muted" style={{ overflowWrap: "anywhere" }}>{p.scope === ArtifactScope.REPO_LOCAL ? p.repository : "User (machine-wide)"}</div>
                            </div>
                          ))
                        )}
                      </td>
                      <td>
                        <StatusCounts counts={countsFromSummary(a.statusCounts)} />
                      </td>
                      <td className="small" style={{ whiteSpace: "nowrap" }}>
                        {a.retractedAt ? (
                          <span className="badge tone-neutral" title={`Retracted ${formatDate(a.retractedAt)}`}>
                            <span className="badge-glyph" aria-hidden="true">–</span>Retracted {formatDate(a.retractedAt)}
                          </span>
                        ) : isPast(a.enforceUntil) ? (
                          <span className="badge tone-muted">
                            <span className="badge-glyph" aria-hidden="true">◷</span>Expired
                          </span>
                        ) : (
                          formatDate(a.enforceUntil)
                        )}
                      </td>
                      <td className="actions">
                        {!a.retractedAt && (
                          <button
                            type="button"
                            className="btn btn-sm btn-danger"
                            onClick={() => setRetracting(a)}
                            aria-label={`Retract ${a.name || a.slug}`}
                          >
                            Retract
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
      <RetractDialog artifact={retracting} onClose={() => setRetracting(null)} />
    </>
  );
}
