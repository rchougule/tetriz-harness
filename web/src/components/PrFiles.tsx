import { useArtifactFiles } from "../api/queries";
import { CopyButton } from "./CopyButton";
import { ErrorState, Loading } from "./States";

export function PrFiles({ artifactId, hasRepoPlacement }: { artifactId: string; hasRepoPlacement: boolean }) {
  const q = useArtifactFiles(artifactId, hasRepoPlacement);
  return (
    <section className="card" aria-labelledby="pr-h">
      <div className="card-head">
        <div className="stack" style={{ gap: 2 }}>
          <h2 id="pr-h">Files for a pull request</h2>
          <p>Commit the artifact into the repository by hand to make it permanent.</p>
        </div>
      </div>
      {!hasRepoPlacement && (
        <div className="card-body">
          <p className="small muted">Only repository placements can be committed. Add a repository placement to list its files.</p>
        </div>
      )}
      {hasRepoPlacement && q.isLoading && <Loading />}
      {hasRepoPlacement && q.error && <ErrorState error={q.error} onRetry={() => void q.refetch()} />}
      {q.data && (
        <div className="card-body stack">
          <div>
            <p className="small muted">{q.data.repositories.length === 1 ? "Repository" : "Repositories"}</p>
            {q.data.repositories.map((r) => (
              <div key={r} className="mono">
                {r}
              </div>
            ))}
          </div>
          <ul className="stack" style={{ listStyle: "none", margin: 0, padding: 0, gap: 8 }}>
            {q.data.files.map((f) => (
              <li key={f.relativePath} className="file-row">
                <span className="path">{f.relativePath}</span>
                {f.isPrimary && <span className="tag">primary</span>}
                <CopyButton text={f.relativePath} label="Copy path" subject={f.relativePath} className="btn btn-sm btn-ghost" />
                <CopyButton text={f.content} label="Copy content" subject={f.relativePath} />
              </li>
            ))}
          </ul>
          <ol className="small" style={{ margin: 0, paddingLeft: 18, color: "var(--fg-2)" }}>
            <li>Create a branch in the repository.</li>
            <li>Place each file at its path with the copied content.</li>
            <li>
              If git ignores a placed file (an agent may list it in <code>.git/info/exclude</code>), add it with <code>git add -f &lt;path&gt;</code>.
            </li>
            <li>Open the pull request. While the artifact stays enforced its status keeps reading Installed.</li>
          </ol>
        </div>
      )}
    </section>
  );
}
