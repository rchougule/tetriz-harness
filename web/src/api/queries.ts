import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  type SetTargetsRequestSchema,
  type UpsertArtifactRequestSchema,
  type GetInstallPreviewRequestSchema,
} from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { useAdmin } from "./context";

export const keys = {
  artifacts: ["artifacts"] as const,
  status: (id: string) => ["status", id] as const,
  files: (id: string) => ["files", id] as const,
  eligible: (repo: string) => ["eligible", repo] as const,
  persons: ["persons"] as const,
  teams: ["teams"] as const,
  preview: (req: unknown) => ["preview", req] as const,
  settings: ["settings"] as const,
  artifact: (id: string) => ["artifact", id] as const,
};

export function useArtifact(artifactId: string) {
  const admin = useAdmin();
  return useQuery({
    queryKey: keys.artifact(artifactId),
    queryFn: () => admin.getArtifact({ artifactId }),
    enabled: artifactId !== "",
  });
}

export function useSettings() {
  const admin = useAdmin();
  return useQuery({ queryKey: keys.settings, queryFn: () => admin.getSettings({}), staleTime: 30_000 });
}

export function useArtifacts(includeRetracted = false) {
  const admin = useAdmin();
  return useQuery({
    queryKey: [...keys.artifacts, { includeRetracted }],
    queryFn: () => admin.listArtifacts({ includeRetracted }),
  });
}

export function useEnforcementStatus(artifactId: string) {
  const admin = useAdmin();
  return useQuery({
    queryKey: keys.status(artifactId),
    queryFn: () => admin.getEnforcementStatus({ artifactId }),
    enabled: artifactId !== "",
  });
}

export function useArtifactFiles(artifactId: string, enabled: boolean) {
  const admin = useAdmin();
  return useQuery({
    queryKey: keys.files(artifactId),
    queryFn: () => admin.getArtifactFiles({ artifactId }),
    enabled,
  });
}

export function useEligibleEngineers(repository: string) {
  const admin = useAdmin();
  return useQuery({
    queryKey: keys.eligible(repository),
    queryFn: () => admin.listEligibleEngineers({ repository }),
  });
}

export function usePersons() {
  const admin = useAdmin();
  return useQuery({ queryKey: keys.persons, queryFn: () => admin.listPersons({}) });
}

export function useTeams() {
  const admin = useAdmin();
  return useQuery({ queryKey: keys.teams, queryFn: () => admin.listTeams({}) });
}

type PreviewReq = MessageInitShape<typeof GetInstallPreviewRequestSchema>;

export function useInstallPreview(req: PreviewReq, enabled: boolean) {
  const admin = useAdmin();
  return useQuery({
    queryKey: keys.preview(req),
    queryFn: () => admin.getInstallPreview(req),
    enabled,
    retry: false,
  });
}

function useInvalidate() {
  const qc = useQueryClient();
  return (...queryKeys: readonly unknown[][]) =>
    Promise.all(queryKeys.map((queryKey) => qc.invalidateQueries({ queryKey })));
}

export function useUpsertArtifact() {
  const admin = useAdmin();
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (req: MessageInitShape<typeof UpsertArtifactRequestSchema>) => admin.upsertArtifact(req),
    onSuccess: (res) => invalidate([...keys.artifacts], [...keys.artifact(res.artifactId)], [...keys.status(res.artifactId)], ["files"]),
  });
}

export function useRemoveArtifact() {
  const admin = useAdmin();
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (artifactId: string) => admin.removeArtifact({ artifactId }),
    onSuccess: () => invalidate([...keys.artifacts], ["artifact"], ["status"]),
  });
}

export function useSetTargets() {
  const admin = useAdmin();
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: (req: MessageInitShape<typeof SetTargetsRequestSchema>) => admin.setTargets(req),
    onSuccess: (_res, req) =>
      invalidate([...keys.artifacts], [...keys.artifact(req.artifactId ?? "")], [...keys.status(req.artifactId ?? "")], [...keys.files(req.artifactId ?? "")]),
  });
}

export function useSetPaused() {
  const admin = useAdmin();
  const invalidate = useInvalidate();
  return useMutation({
    mutationFn: async (paused: boolean): Promise<boolean> =>
      (paused ? await admin.pauseEnforcement({}) : await admin.resumeEnforcement({})).paused,
    onSuccess: () => invalidate([...keys.settings], [...keys.artifacts]),
  });
}
