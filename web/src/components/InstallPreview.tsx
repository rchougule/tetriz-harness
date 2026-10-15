import { ArtifactScope, ArtifactType, type IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { useInstallPreview } from "../api/queries";
import { errorMessage } from "../api/errors";
import { SLUG_RE } from "../lib/validate";
import { useDebounced } from "../lib/useDebounced";

interface Props {
  ideType: IdeType;
  type: ArtifactType;
  slug: string;
  filePaths: string[];
  scope: ArtifactScope;
}

const SCOPE_CAPTION: Partial<Record<ArtifactScope, string>> = {
  [ArtifactScope.USER_GLOBAL]: "User (machine-wide)",
  [ArtifactScope.REPO_LOCAL]: "Each checkout of the repository",
};

export function InstallPreview({ ideType, type, slug, filePaths, scope }: Props) {
  const req = useDebounced({ ideType, type, slug, scope, fileRelativePaths: type === ArtifactType.SKILL ? filePaths : [] });
  const ready = req.ideType !== 0 && req.type !== 0 && SLUG_RE.test(req.slug);
  const preview = useInstallPreview(req, ready);

  return (
    <div className="stack" style={{ gap: 8 }}>
      <p className="small muted">{SCOPE_CAPTION[scope] ?? "Placement"}</p>
      {!ready && <p className="small muted">Choose a tool, a type and a valid slug to see where files land.</p>}
      {ready && preview.isLoading && <p className="small muted">Deriving paths…</p>}
      {ready && preview.error && (
        <p className="small" style={{ color: "var(--caution-fg)" }}>
          {errorMessage(preview.error)}
        </p>
      )}
      {ready && preview.data && (
        <ul style={{ listStyle: "none", margin: 0, padding: 0 }} className="stack" aria-label={`Install paths: ${SCOPE_CAPTION[scope] ?? "placement"}`}>
          {preview.data.files.map((f) => (
            <li key={f.relativePath} className="row" style={{ gap: 6, flexWrap: "nowrap" }}>
              <code style={{ wordBreak: "break-all" }}>{preview.data.root ? `${preview.data.root}/${f.relativePath}` : f.relativePath}</code>
              {f.isPrimary && <span className="tag">primary</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
