import { useState } from "react";
import { ArtifactScope, type Placement } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import type { ArtifactSummary } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { useSetTargets } from "../api/queries";
import { errorMessage } from "../api/errors";
import { audienceToDraft, draftToAudience } from "../lib/audience";
import { SCOPE_OPTIONS } from "../lib/labels";
import { placementUnsupported, validateAudience, validateRepository, type AudienceDraft } from "../lib/validate";
import { AudiencePicker } from "./AudiencePicker";
import { Dialog } from "./Dialog";
import { Field } from "./Field";
import { useToast } from "./Toast";

interface Props {
  artifact: ArtifactSummary;
  // null adds a placement; an existing one is edited in place, keyed by its scope and repository.
  placement: Placement | null;
  onClose: () => void;
}

export function TargetsDialog({ artifact, placement, onClose }: Props) {
  const isNew = placement === null;
  const [scope, setScope] = useState<ArtifactScope>(placement?.scope ?? ArtifactScope.USER_GLOBAL);
  const [repository, setRepository] = useState(placement?.repository ?? "");
  const [audience, setAudience] = useState<AudienceDraft>(audienceToDraft(placement?.audience));
  const [errors, setErrors] = useState<{ scope?: string; repository?: string; audience?: string }>({});
  const setTargets = useSetTargets();
  const toast = useToast();

  const save = () => {
    const e: typeof errors = {};
    if (scope === ArtifactScope.REPO_LOCAL) e.repository = validateRepository(repository);
    e.scope = placementUnsupported(artifact.ideType, artifact.type, scope);
    if (isNew && artifact.placements.some((p) => p.scope === scope && p.repository === (scope === ArtifactScope.REPO_LOCAL ? repository : "")))
      e.scope = "That placement already exists; edit it instead.";
    e.audience = validateAudience(audience);
    setErrors(e);
    if (e.scope || e.repository || e.audience) return;
    setTargets.mutate(
      {
        artifactId: artifact.artifactId,
        scope,
        repository: scope === ArtifactScope.REPO_LOCAL ? repository : "",
        audience: draftToAudience(audience),
      },
      {
        onSuccess: (res) => {
          toast(`Targets saved. Primary file lands at ${res.installPath}`);
          onClose();
        },
      },
    );
  };

  return (
    <Dialog
      open
      wide
      title={isNew ? "Add placement" : "Edit targets"}
      description="Saving replaces this placement's audience and restarts the enforcement window. Other placements are untouched."
      onClose={onClose}
      busy={setTargets.isPending}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={setTargets.isPending}>
            Cancel
          </button>
          <button type="button" className="btn btn-primary" onClick={save} disabled={setTargets.isPending}>
            {setTargets.isPending ? "Saving…" : "Save targets"}
          </button>
        </>
      }
    >
      <div className="stack">
        <fieldset disabled={!isNew}>
          <legend>Where</legend>
          <div className="choice-row" role="radiogroup">
            {SCOPE_OPTIONS.map((o) => (
              <label key={o.value} className="choice">
                <input type="radio" name="t-scope" checked={scope === o.value} onChange={() => setScope(o.value)} />
                <span>
                  <span style={{ fontWeight: 560 }}>{o.label}</span>
                  <br />
                  <span className="small muted">{o.hint}</span>
                </span>
              </label>
            ))}
          </div>
          {!isNew && <p className="small muted" style={{ marginTop: 6 }}>To move it, add a new placement and remove this one.</p>}
          {errors.scope && <p className="small" style={{ color: "var(--danger-fg)", marginTop: 6 }}>{errors.scope}</p>}
        </fieldset>
        {scope === ArtifactScope.REPO_LOCAL && (
          <Field label="Repository" error={errors.repository} hint="owner/repo">
            {(p) => (
              <input
                {...p}
                className="input mono"
                value={repository}
                readOnly={!isNew}
                placeholder="example-org/example-repo"
                spellCheck={false}
                onChange={(e) => setRepository(e.target.value.trim())}
              />
            )}
          </Field>
        )}
        <AudiencePicker value={audience} onChange={setAudience} repository={scope === ArtifactScope.REPO_LOCAL ? repository : ""} error={errors.audience} />
        {setTargets.error && (
          <p role="alert" className="callout callout-danger">
            {errorMessage(setTargets.error)}
          </p>
        )}
      </div>
    </Dialog>
  );
}
