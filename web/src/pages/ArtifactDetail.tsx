import { useState } from "react";
import { Link, useParams } from "react-router";
import { ArtifactScope, ArtifactType, type Placement } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { useArtifact, useSetTargets } from "../api/queries";
import { errorMessage, isNotFound } from "../api/errors";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { EnforcementWindowNote } from "../components/EnforcementWindowNote";
import { InstallPreview } from "../components/InstallPreview";
import { PrFiles } from "../components/PrFiles";
import { RetractDialog } from "../components/RetractDialog";
import { ErrorState, Loading } from "../components/States";
import { StatusTable } from "../components/StatusTable";
import { TargetsDialog } from "../components/TargetsDialog";
import { useToast } from "../components/Toast";
import { audienceSummary } from "../lib/audience";
import { formatDateTime, isPast } from "../lib/format";
import { ideLabel, scopeLabel, typeLabel } from "../lib/labels";
import { useNames } from "../lib/names";

export function ArtifactDetailPage() {
  const { id = "" } = useParams();
  const stored = useArtifact(id);
  const names = useNames();
  const toast = useToast();
  const setTargets = useSetTargets();
  const [editing, setEditing] = useState<Placement | null | undefined>(undefined);
  const [removing, setRemoving] = useState<Placement | null>(null);
  const [retracting, setRetracting] = useState(false);

  if (stored.isLoading) return <Loading />;
  if (stored.error && !isNotFound(stored.error)) return <ErrorState error={stored.error} onRetry={() => void stored.refetch()} />;
  const a = stored.data?.artifact;
  if (!a)
    return (
      <div className="card card-body stack">
        <p>This artifact does not exist.</p>
        <Link to="/artifacts">Back to artifacts</Link>
      </div>
    );

  const retracted = a.retractedAt !== undefined;
  const expired = !retracted && isPast(a.enforceUntil);
  const hasRepo = a.placements.some((p) => p.scope === ArtifactScope.REPO_LOCAL);
  const title = a.name || a.slug;
  const placed = [...new Set(a.placements.map((p) => p.scope))].sort();
  const previewScopes = placed.length ? placed : [ArtifactScope.USER_GLOBAL, ArtifactScope.REPO_LOCAL];

  return (
    <>
      <div className="crumb">
        <Link to="/artifacts">Artifacts</Link> / {title}
      </div>
      <div className="page-head">
        <div className="stack" style={{ gap: 6 }}>
          <h1>{title}</h1>
          <div className="row">
            <span className="tag">{typeLabel(a.type)}</span>
            <span className="tag">{ideLabel(a.ideType)}</span>
            <code className="muted">{a.slug}</code>
          </div>
        </div>
        {!retracted && (
          <div className="row">
            <Link to={`/artifacts/${a.artifactId}/edit`} className="btn">
              Edit
            </Link>
            <button type="button" className="btn btn-danger" onClick={() => setRetracting(true)}>
              Retract
            </button>
          </div>
        )}
      </div>

      {retracted && (
        <div className="callout callout-info" role="status" style={{ marginBottom: 16 }}>
          <span className="icon" aria-hidden="true">–</span>
          <span>
            <strong>Retracted on {formatDateTime(a.retractedAt)}.</strong> Nobody receives it any more. Status below tracks whether each
            person's machine still reports the file. The slug is free for a new artifact.
          </span>
        </div>
      )}

      {expired && (
        <div className="callout callout-info" role="status" style={{ marginBottom: 16 }}>
          <span className="icon" aria-hidden="true">◷</span>
          <span>
            <strong>The enforcement window has closed.</strong> Agents leave the files exactly as they are. Save the artifact or change its targets to
            enforce it again.
          </span>
        </div>
      )}

      <div className="detail-grid">
        <div className="stack">
          <section className="card" aria-labelledby="targets-h">
            <div className="card-head">
              <div className="stack" style={{ gap: 2 }}>
                <h2 id="targets-h">Targets</h2>
                <p>
                  {retracted
                    ? "The placements its retraction removed."
                    : "Each placement is one scope and repository with its own audience. Teams resolve to members when status is read."}
                </p>
              </div>
              <div className="spacer" />
              {!retracted && (
                <button type="button" className="btn btn-sm" onClick={() => setEditing(null)}>
                  Add placement
                </button>
              )}
            </div>
            {a.placements.length === 0 ? (
              <div className="card-body">
                <p className="muted small">Not targeted. The definition exists but nobody receives it.</p>
              </div>
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Where</th>
                      <th scope="col">Who</th>
                      {!retracted && (
                        <th scope="col">
                          <span className="visually-hidden">Actions</span>
                        </th>
                      )}
                    </tr>
                  </thead>
                  <tbody>
                    {a.placements.map((p) => {
                      const where = scopeLabel(p.scope, p.repository);
                      return (
                        <tr key={`${p.scope}:${p.repository}`}>
                          <td>
                            <div>{p.scope === ArtifactScope.REPO_LOCAL ? "Repository" : "User (machine-wide)"}</div>
                            {p.repository && <div className="cell-sub">{p.repository}</div>}
                          </td>
                          <td>{audienceSummary(p.audience, names)}</td>
                          {!retracted && (
                            <td className="actions">
                              <div className="row" style={{ justifyContent: "flex-end", flexWrap: "nowrap" }}>
                                <button type="button" className="btn btn-sm" onClick={() => setEditing(p)} aria-label={`Edit targets for ${where}`}>
                                  Edit
                                </button>
                                <button type="button" className="btn btn-sm btn-danger" onClick={() => setRemoving(p)} aria-label={`Remove placement ${where}`}>
                                  Remove
                                </button>
                              </div>
                            </td>
                          )}
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </div>

        <aside className="stack">
          <section className="card" aria-labelledby="about-h">
            <div className="card-head">
              <h2 id="about-h">Definition</h2>
            </div>
            <div className="card-body stack">
              <dl className="kv">
                <dt>Updated</dt>
                <dd>{formatDateTime(a.updatedAt)}</dd>
                <dt>Content hash</dt>
                <dd>
                  <code title={a.primaryHash}>{a.primaryHash.slice(0, 12) || "—"}</code>
                </dd>
                {a.type === ArtifactType.HOOK && (
                  <>
                    <dt>Hook event</dt>
                    <dd>
                      <code>{a.hookConfig?.event || "—"}</code>
                    </dd>
                    <dt>Matcher</dt>
                    <dd>
                      <code>{a.hookConfig?.matcher || "any"}</code>
                    </dd>
                    <dt>Timeout</dt>
                    <dd>{a.hookConfig?.timeoutSeconds ? `${a.hookConfig.timeoutSeconds}s` : "Tool default"}</dd>
                  </>
                )}
              </dl>
              {!retracted && <EnforcementWindowNote enforceUntil={a.enforceUntil} />}
            </div>
          </section>
          <section className="card" aria-labelledby="preview-h">
            <div className="card-head">
              <h2 id="preview-h">Install preview</h2>
            </div>
            <div className="card-body">
              <div className="stack">
                {previewScopes.map((scope) => (
                  <InstallPreview key={scope} ideType={a.ideType} type={a.type} slug={a.slug} filePaths={[]} scope={scope} />
                ))}
              </div>
            </div>
          </section>
        </aside>
      </div>
      <div className="stack" style={{ marginTop: 16 }}>
        <StatusTable artifactId={a.artifactId} />
        {!retracted && <PrFiles artifactId={a.artifactId} hasRepoPlacement={hasRepo} />}
      </div>

      {editing !== undefined && <TargetsDialog artifact={a} placement={editing} onClose={() => setEditing(undefined)} />}
      <ConfirmDialog
        open={removing !== null}
        title="Remove this placement?"
        description={removing ? `${scopeLabel(removing.scope, removing.repository)} stops being enforced. Agents remove its unmodified files on their next syncs; a file someone edited is left in place.` : undefined}
        confirmLabel="Remove placement"
        danger
        pending={setTargets.isPending}
        error={setTargets.error ? errorMessage(setTargets.error) : null}
        onCancel={() => {
          setTargets.reset();
          setRemoving(null);
        }}
        onConfirm={() => {
          if (!removing) return;
          setTargets.mutate(
            { artifactId: a.artifactId, scope: removing.scope, repository: removing.repository, audience: {} },
            {
              onSuccess: () => {
                toast("Placement removed");
                setRemoving(null);
              },
            },
          );
        }}
      />
      <RetractDialog artifact={retracting ? a : null} onClose={() => setRetracting(false)} />
    </>
  );
}
