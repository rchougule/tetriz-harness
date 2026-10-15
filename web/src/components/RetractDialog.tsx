import type { ArtifactSummary } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { useRemoveArtifact } from "../api/queries";
import { errorMessage } from "../api/errors";
import { ConfirmDialog } from "./ConfirmDialog";
import { useToast } from "./Toast";

interface Props {
  artifact: ArtifactSummary | null;
  onClose: () => void;
}

export function RetractDialog({ artifact, onClose }: Props) {
  const remove = useRemoveArtifact();
  const toast = useToast();
  const close = () => {
    remove.reset();
    onClose();
  };
  return (
    <ConfirmDialog
      open={artifact !== null}
      title={`Retract ${artifact?.name ?? "artifact"}?`}
      description="Agents stop enforcing it and remove its unmodified files from machines on their next syncs. A file someone edited is left in place and shows as Removal pending."
      confirmLabel="Retract"
      danger
      pending={remove.isPending}
      error={remove.error ? errorMessage(remove.error) : null}
      onCancel={close}
      onConfirm={() => {
        if (!artifact) return;
        remove.mutate(artifact.artifactId, {
          onSuccess: () => {
            toast(`Retracted ${artifact.name}`);
            close();
          },
        });
      }}
    >
      {artifact && (
        <p className="small muted">
          <span className="mono">{artifact.slug}</span> moves to the retracted list, where its removal can be followed. To stop it on specific
          machines instead, change its targets.
        </p>
      )}
    </ConfirmDialog>
  );
}
