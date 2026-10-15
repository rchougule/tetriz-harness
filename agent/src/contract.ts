// The engine's view of the control plane. Wire-agnostic: `transport/connect.ts` is the only file that
// maps these to and from the generated harnessctl.v1 messages. Enum-valued fields carry the proto
// value names, so persisted state and fixtures read the same as the API.

export const SCOPE_USER_GLOBAL = "ARTIFACT_SCOPE_USER_GLOBAL";
export const SCOPE_REPO_LOCAL = "ARTIFACT_SCOPE_REPO_LOCAL";

export const TYPE_HOOK = "ARTIFACT_TYPE_HOOK";

export const IDE_CLAUDE_CODE = "IDE_TYPE_CLAUDE_CODE";
export const IDE_CODEX = "IDE_TYPE_CODEX";
export const IDE_CURSOR = "IDE_TYPE_CURSOR";

/** One file placed for a directory artifact; `relativePath` is scope-root-relative. */
export interface EnforcedArtifactFileRef {
  relativePath: string;
  /** The key a body is fetched by; the harnessctl API addresses bodies by content hash, so it equals `hash`. */
  fileId: string;
  hash: string;
  executable: boolean;
}

export interface HookConfig {
  event: string;
  matcher: string;
  timeoutSeconds: number;
}

export interface EnforcedArtifact {
  artifactId: string;
  ideType: string;
  type: string;
  name: string;
  scope: string;
  /** "owner/repo" a REPO_LOCAL artifact targets, matched against each checkout's remote; "" for USER_GLOBAL. */
  repoFullName: string;
  installPath: string;
  /** Body key of the primary file (see `EnforcedArtifactFileRef.fileId`). */
  primaryFileId: string;
  primaryHash: string;
  hookConfig?: HookConfig;
  hookCommand: string;
  /** Full placed file set; absent means the scalar primary is the whole artifact. */
  files?: EnforcedArtifactFileRef[];
  /** Place the files but leave the hook's activation entry unmerged (absent = active). */
  inactive?: boolean;
  /** Close of the enforcement window (RFC3339); display only. */
  enforceUntil?: string;
  /** The window has closed: the agent leaves the artifact exactly as it is. */
  expired?: boolean;
}

export interface EnforcedSet {
  artifacts: EnforcedArtifact[];
  /** The org-wide kill switch: make no writes, and do not read the empty list as removal. */
  paused?: boolean;
}

/** Where to get one body: a URL to GET, or the bytes inline. Either is hash-verified before use. */
export interface DownloadRef {
  fileId: string;
  url?: string;
  bytes?: Uint8Array;
}

export interface ApplyFailure {
  artifactId: string;
  /** "owner/repo" of the failing placement; "" for USER_GLOBAL. */
  repository: string;
  /** The primary hash being applied; the server ignores a failure for any other hash. */
  contentHash: string;
  reason: string;
}

/** One file the agent manages, as found on disk. */
export interface InstalledFile {
  ide: string;
  /** "owner/repo" for a repo-local placement, "" for a user-global one. */
  repo: string;
  /** Opaque, stable key of the checkout (never its path); "" for a user-global placement. */
  checkout: string;
  sourcePath: string;
  contentHash: string;
}

export interface StateReport {
  deviceId: string;
  agentVersion: string;
  /** Every "owner/repo" with a checkout on this machine; gates repo-local eligibility. */
  knownRepositories: string[];
  installed: InstalledFile[];
  applyFailures: ApplyFailure[];
}

/** Server-side cap on content hashes per body request. */
export const DOWNLOAD_BATCH_MAX = 100;

/** Agent-side cap on a failure reason's length (the server truncates at a larger bound). */
export const FAILURE_REASON_MAX = 500;
