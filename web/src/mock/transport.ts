import { Code, ConnectError, createRouterTransport, type Interceptor, type ServiceImpl, type Transport } from "@connectrpc/connect";
import { timestampDate, timestampFromDate, type Timestamp } from "@bufbuild/protobuf/wkt";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { AdminService, type ArtifactFile } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import {
  ArtifactScope,
  ArtifactType,
  EnforcementStatus,
  IdeType,
  type AudienceSchema,
  type HookConfigSchema,
} from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { authInterceptor } from "../api/transport";
import { fileInstallPath, installPath } from "./derive";

type AudienceInit = MessageInitShape<typeof AudienceSchema>;

interface MockPlacement {
  scope: ArtifactScope;
  repository: string;
  audience: { orgWide: boolean; personIds: string[]; teamIds: string[] };
}

interface MockArtifact {
  artifactId: string;
  ideType: IdeType;
  type: ArtifactType;
  slug: string;
  name: string;
  inline: string;
  files: { relativePath: string; content: string }[];
  hook?: MessageInitShape<typeof HookConfigSchema>;
  placements: MockPlacement[];
  updatedAt: Date;
  enforceUntil: Date;
  // Seeded outcomes keyed by `${personId}|${repository}` so each screen shows every status.
  outcomes: Record<string, { status: EnforcementStatus; detail?: string; observedAt?: Date }>;
  retractedAt?: Date;
}

interface MockPerson {
  personId: string;
  displayName: string;
  reported: boolean;
  repositories: string[];
}

const DAY = 86_400_000;
const WINDOW_DAYS = 15;
const MAX_WINDOW_DAYS = 90;

function hash(s: string): string {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 0x01000193);
  return (h >>> 0).toString(16).padStart(8, "0").repeat(8);
}

function ts(d: Date | undefined): Timestamp | undefined {
  return d ? timestampFromDate(d) : undefined;
}

export interface MockOptions {
  latencyMs?: number;
  now?: () => Date;
  // hooksEnabled mirrors the server's HOOK opt-in; defaults to on.
  hooksEnabled?: boolean;
}

export function createMockState(opts: MockOptions = {}) {
  const now = opts.now ?? (() => new Date());
  const t0 = now().getTime();
  const ago = (days: number) => new Date(t0 - days * DAY);
  let seq = 100;
  const id = (p: string) => `${p}_${(seq++).toString(36)}`;

  const persons: MockPerson[] = [
    { personId: "p_ada", displayName: "Ada Lovelace", reported: true, repositories: ["example-org/example-repo", "example-org/docs-site"] },
    { personId: "p_alan", displayName: "Alan Turing", reported: true, repositories: ["example-org/example-repo"] },
    { personId: "p_grace", displayName: "Grace Hopper", reported: true, repositories: ["example-org/example-repo"] },
    { personId: "p_edsger", displayName: "Edsger Dijkstra", reported: true, repositories: [] },
    { personId: "p_barbara", displayName: "Barbara Liskov", reported: true, repositories: ["example-org/docs-site"] },
    { personId: "p_katherine", displayName: "Katherine Johnson", reported: false, repositories: [] },
  ];
  const teams = [
    { teamId: "t_platform", name: "Platform", memberPersonIds: ["p_ada", "p_alan", "p_edsger"] },
    { teamId: "t_mobile", name: "Mobile", memberPersonIds: ["p_grace", "p_barbara"] },
    { teamId: "t_docs", name: "Docs", memberPersonIds: [] as string[] },
  ];

  const artifacts: MockArtifact[] = [
    {
      artifactId: "a_review",
      ideType: IdeType.CLAUDE_CODE,
      type: ArtifactType.SKILL,
      slug: "code-review",
      name: "Code review checklist",
      inline: "",
      files: [
        { relativePath: "SKILL.md", content: "---\nname: code-review\ndescription: Review a diff against the team checklist.\n---\n\n# Code review\n\n1. Read the whole diff before commenting.\n2. Run the tests.\n" },
        { relativePath: "scripts/diffstat.sh", content: "#!/bin/sh\ngit diff --stat \"$@\"\n" },
      ],
      placements: [{ scope: ArtifactScope.USER_GLOBAL, repository: "", audience: { orgWide: true, personIds: [], teamIds: [] } }],
      updatedAt: ago(3),
      enforceUntil: new Date(t0 + 27 * DAY),
      outcomes: {
        "p_ada|": { status: EnforcementStatus.INSTALLED, observedAt: ago(0.1) },
        "p_alan|": { status: EnforcementStatus.INSTALLED, observedAt: ago(0.4) },
        "p_grace|": { status: EnforcementStatus.FAILED, detail: "permission denied writing ~/.claude/skills/code-review/SKILL.md", observedAt: ago(0.2) },
        "p_barbara|": { status: EnforcementStatus.PENDING },
      },
    },
    {
      artifactId: "a_commit",
      ideType: IdeType.CURSOR,
      type: ArtifactType.RULE,
      slug: "commit-style",
      name: "Commit message style",
      inline: "---\ndescription: Commit message conventions\nalwaysApply: true\n---\n\nWrite commit subjects in the imperative mood, at most 72 characters.\n",
      files: [],
      placements: [{ scope: ArtifactScope.REPO_LOCAL, repository: "example-org/example-repo", audience: { orgWide: false, personIds: [], teamIds: ["t_platform"] } }],
      updatedAt: ago(8),
      enforceUntil: new Date(t0 + 22 * DAY),
      outcomes: {
        "p_ada|example-org/example-repo": { status: EnforcementStatus.INSTALLED, observedAt: ago(0.3) },
        "p_alan|example-org/example-repo": { status: EnforcementStatus.PENDING },
      },
    },
    {
      artifactId: "a_guide",
      ideType: IdeType.CLAUDE_CODE,
      type: ArtifactType.CLAUDE_MD,
      slug: "repo-guide",
      name: "Repository guide",
      inline: "# example-repo\n\nRun `make test` before every push. Generated code lives in gen/ and is never edited by hand.\n",
      files: [],
      placements: [{ scope: ArtifactScope.REPO_LOCAL, repository: "example-org/example-repo", audience: { orgWide: false, personIds: ["p_ada", "p_grace"], teamIds: [] } }],
      updatedAt: ago(1),
      enforceUntil: new Date(t0 + 29 * DAY),
      outcomes: { "p_ada|example-org/example-repo": { status: EnforcementStatus.INSTALLED, observedAt: ago(0.05) } },
    },
    {
      artifactId: "a_secrets",
      ideType: IdeType.CLAUDE_CODE,
      type: ArtifactType.HOOK,
      slug: "block-secrets",
      name: "Block committed secrets",
      inline: "#!/bin/sh\n# Reads the tool call on stdin and refuses writes that look like credentials.\ngrep -qE 'BEGIN (RSA|EC) PRIVATE KEY' && exit 2\nexit 0\n",
      files: [],
      hook: { event: "PreToolUse", matcher: "Write|Edit", timeoutSeconds: 10 },
      placements: [{ scope: ArtifactScope.USER_GLOBAL, repository: "", audience: { orgWide: false, personIds: [], teamIds: ["t_mobile"] } }],
      updatedAt: ago(5),
      enforceUntil: new Date(t0 + 25 * DAY),
      outcomes: { "p_grace|": { status: EnforcementStatus.INSTALLED, observedAt: ago(1) } },
    },
    {
      artifactId: "a_notes",
      ideType: IdeType.KIRO,
      type: ArtifactType.AGENT,
      slug: "release-notes",
      name: "Release notes writer",
      inline: "---\nname: release-notes\n---\n\nSummarise merged changes since the last tag into user-facing release notes.\n",
      files: [],
      placements: [{ scope: ArtifactScope.USER_GLOBAL, repository: "", audience: { orgWide: false, personIds: ["p_alan", "p_barbara"], teamIds: [] } }],
      updatedAt: ago(45),
      enforceUntil: ago(15),
      outcomes: {},
    },
  ];

  artifacts.push({
    artifactId: "a_legacy",
    ideType: IdeType.CURSOR,
    type: ArtifactType.RULE,
    slug: "legacy-lint",
    name: "Legacy lint rules",
    inline: "---\ndescription: Superseded lint conventions\n---\n\nPrefer the shared lint config.\n",
    files: [],
    placements: [{ scope: ArtifactScope.USER_GLOBAL, repository: "", audience: { orgWide: false, personIds: [], teamIds: ["t_mobile"] } }],
    updatedAt: ago(6),
    enforceUntil: new Date(t0 + 9 * DAY),
    outcomes: {
      "p_grace|": {
        status: EnforcementStatus.INSTALLED,
        observedAt: ago(0.2),
        detail: "left in place: modified since installed: .cursor/rules/legacy-lint.mdc",
      },
    },
    retractedAt: ago(2),
  });

  let paused = false;
  const hooksEnabled = opts.hooksEnabled ?? true;

  const personById = (pid: string) => persons.find((p) => p.personId === pid);

  function resolve(a: MockPlacement["audience"]): string[] {
    if (a.orgWide) return persons.map((p) => p.personId);
    const ids = new Set(a.personIds);
    for (const tid of a.teamIds) for (const m of teams.find((t) => t.teamId === tid)?.memberPersonIds ?? []) ids.add(m);
    return [...ids].filter((pid) => personById(pid));
  }

  function statusesFor(a: MockArtifact) {
    const expired = a.enforceUntil.getTime() < now().getTime();
    const rows = [];
    for (const pl of a.placements) {
      for (const pid of resolve(pl.audience)) {
        const p = personById(pid)!;
        const key = `${pid}|${pl.repository}`;
        const o = a.outcomes[key];
        let status = EnforcementStatus.PENDING;
        let detail = "";
        let observedAt: Date | undefined;
        if (a.retractedAt) {
          // Retraction grades presence only: a still-reported file is pending removal.
          const present = o?.status === EnforcementStatus.INSTALLED;
          status = present ? EnforcementStatus.REMOVAL_PENDING : EnforcementStatus.REMOVED;
          observedAt = present ? o?.observedAt : undefined;
          detail = present ? (o?.detail ?? "") : "";
        } else if (expired) status = EnforcementStatus.EXPIRED;
        else if (!p.reported) {
          status = EnforcementStatus.EXCLUDED;
          detail = "no agent has reported for this person";
        } else if (pl.scope === ArtifactScope.REPO_LOCAL && !p.repositories.includes(pl.repository)) {
          status = EnforcementStatus.EXCLUDED;
          detail = "no known checkout of this repository";
        } else if (o) {
          status = o.status;
          detail = o.detail ?? "";
          observedAt = o.observedAt;
        }
        rows.push({ personId: pid, scope: pl.scope, repository: pl.repository, status, detail, observedAt: ts(observedAt) });
      }
    }
    return rows;
  }

  function stamp(a: MockArtifact, until?: Date) {
    a.updatedAt = now();
    a.enforceUntil = until ?? new Date(now().getTime() + WINDOW_DAYS * DAY);
    a.outcomes = {};
  }

  function primaryHash(a: MockArtifact) {
    return hash(a.files.find((f) => f.relativePath === "SKILL.md")?.content ?? a.inline);
  }

  function summary(a: MockArtifact) {
    return {
      artifactId: a.artifactId, ideType: a.ideType, type: a.type, slug: a.slug, name: a.name,
      primaryHash: primaryHash(a), hookConfig: a.hook,
      placements: [...a.placements].sort((x, y) => x.scope - y.scope || x.repository.localeCompare(y.repository)),
      updatedAt: ts(a.updatedAt), enforceUntil: ts(a.enforceUntil),
      statusCounts: tally(statusesFor(a)),
      retractedAt: ts(a.retractedAt),
    };
  }

  function tally(rows: { status: EnforcementStatus }[]) {
    const n = (s: EnforcementStatus) => rows.filter((r) => r.status === s).length;
    return {
      installed: n(EnforcementStatus.INSTALLED), pending: n(EnforcementStatus.PENDING), failed: n(EnforcementStatus.FAILED),
      excluded: n(EnforcementStatus.EXCLUDED), removalPending: n(EnforcementStatus.REMOVAL_PENDING), removed: n(EnforcementStatus.REMOVED),
      expired: n(EnforcementStatus.EXPIRED),
    };
  }

  function mustArtifact(artifactId: string, includeRetracted = false) {
    const a = artifacts.find((x) => x.artifactId === artifactId && (includeRetracted || !x.retractedAt));
    if (!a) throw new ConnectError("artifact not found", Code.NotFound);
    return a;
  }

  function toAudience(a: AudienceInit | undefined) {
    return { orgWide: a?.orgWide ?? false, personIds: [...(a?.personIds ?? [])], teamIds: [...(a?.teamIds ?? [])] };
  }

  const impl: ServiceImpl<typeof AdminService> = {
    upsertArtifact(req) {
      if (!/^[a-z0-9][a-z0-9-]*$/.test(req.slug) || req.slug.length > 100)
        throw new ConnectError("slug must match ^[a-z0-9][a-z0-9-]*$", Code.InvalidArgument);
      if (req.files.length > 0 && req.type !== ArtifactType.SKILL)
        throw new ConnectError("only a skill may carry files", Code.InvalidArgument);
      if (req.type === ArtifactType.HOOK && !hooksEnabled)
        throw new ConnectError("hook artifacts are disabled on this server; set HARNESS_ALLOW_HOOKS=true to allow them", Code.FailedPrecondition);
      if (req.type === ArtifactType.HOOK && !req.hookConfig?.event)
        throw new ConnectError("hook_config is required for a hook", Code.InvalidArgument);
      const until = req.enforceUntil ? timestampDate(req.enforceUntil) : undefined;
      if (until && until.getTime() <= now().getTime()) throw new ConnectError("enforce_until must be in the future", Code.InvalidArgument);
      if (until && until.getTime() > now().getTime() + MAX_WINDOW_DAYS * DAY)
        throw new ConnectError(`enforce_until must be at most ${MAX_WINDOW_DAYS} days ahead`, Code.InvalidArgument);
      let a = req.artifactId
        ? mustArtifact(req.artifactId)
        : artifacts.find((x) => !x.retractedAt && x.ideType === req.ideType && x.type === req.type && x.slug === req.slug);
      if (!a) {
        a = {
          artifactId: id("a"), ideType: req.ideType, type: req.type, slug: req.slug, name: req.name,
          inline: "", files: [], placements: [], updatedAt: now(), enforceUntil: now(), outcomes: {},
        };
        artifacts.push(a);
      }
      a.slug = req.slug;
      a.name = req.name;
      if (req.inlineContent) { a.inline = req.inlineContent; a.files = []; }
      if (req.files.length) { a.files = req.files.map((f: ArtifactFile) => ({ relativePath: f.relativePath, content: f.content })); a.inline = ""; }
      a.hook = req.type === ArtifactType.HOOK ? { event: req.hookConfig?.event, matcher: req.hookConfig?.matcher, timeoutSeconds: req.hookConfig?.timeoutSeconds } : undefined;
      stamp(a, until);
      return { artifactId: a.artifactId, enforceUntil: ts(a.enforceUntil) };
    },
    listArtifacts(req) {
      const live = artifacts.filter((a) => !a.retractedAt);
      const retracted = req.includeRetracted
        ? artifacts.filter((a) => a.retractedAt).sort((x, y) => y.retractedAt!.getTime() - x.retractedAt!.getTime())
        : [];
      return { enforcementPaused: paused, artifacts: [...live, ...retracted].map(summary) };
    },
    getArtifact(req) {
      const a = mustArtifact(req.artifactId, true);
      const primary = a.files.find((f) => f.relativePath === "SKILL.md")?.content ?? a.inline;
      const files =
        a.type === ArtifactType.SKILL
          ? (a.files.length ? a.files : [{ relativePath: "SKILL.md", content: a.inline }])
              .map((f) => ({ ...f }))
              .sort((x, y) => Number(y.relativePath === "SKILL.md") - Number(x.relativePath === "SKILL.md") || x.relativePath.localeCompare(y.relativePath))
          : [];
      return { artifact: summary(a), inlineContent: primary, files };
    },
    removeArtifact(req) {
      mustArtifact(req.artifactId).retractedAt = now();
      return {};
    },
    setTargets(req) {
      const a = mustArtifact(req.artifactId);
      const repoLocal = req.scope === ArtifactScope.REPO_LOCAL;
      if (repoLocal && !/^[^/\s]+\/[^/\s]+$/.test(req.repository)) throw new ConnectError("repository must be owner/repo", Code.InvalidArgument);
      if (!repoLocal && req.repository) throw new ConnectError("repository is rejected for USER_GLOBAL", Code.InvalidArgument);
      const path = installPath(a.ideType, a.type, a.slug, req.scope);
      if (!path) throw new ConnectError("no install path for this tool, type and scope", Code.FailedPrecondition);
      const audience = toAudience(req.audience);
      const empty = !audience.orgWide && audience.personIds.length === 0 && audience.teamIds.length === 0;
      a.placements = a.placements.filter((p) => !(p.scope === req.scope && p.repository === req.repository));
      if (!empty) a.placements.push({ scope: req.scope, repository: req.repository, audience });
      a.enforceUntil = new Date(Math.max(a.enforceUntil.getTime(), now().getTime() + WINDOW_DAYS * DAY));
      return { installPath: path, enforceUntil: ts(a.enforceUntil) };
    },
    getEnforcementStatus(req) {
      return { statuses: statusesFor(mustArtifact(req.artifactId, true)) };
    },
    listEligibleEngineers(req) {
      return {
        engineers: persons.map((p) => {
          let reason = "";
          if (!p.reported) reason = "no agent has reported for this person";
          else if (req.repository && !p.repositories.includes(req.repository)) reason = "no known checkout of this repository";
          return { personId: p.personId, displayName: p.displayName, eligible: reason === "", ineligibleReason: reason };
        }),
      };
    },
    getArtifactFiles(req) {
      const a = mustArtifact(req.artifactId);
      const repos = a.placements.filter((p) => p.scope === ArtifactScope.REPO_LOCAL).map((p) => p.repository);
      if (repos.length === 0) throw new ConnectError("definition has no repository placement", Code.FailedPrecondition);
      const root = installPath(a.ideType, a.type, a.slug, ArtifactScope.REPO_LOCAL) ?? a.slug;
      const files = a.files.length
        ? a.files.map((f) => ({ relativePath: fileInstallPath(root, f.relativePath), content: f.content, isPrimary: f.relativePath === "SKILL.md" }))
        : [{ relativePath: root, content: a.inline, isPrimary: true }];
      files.sort((x, y) => Number(y.isPrimary) - Number(x.isPrimary) || x.relativePath.localeCompare(y.relativePath));
      return { files, repositories: repos };
    },
    getInstallPreview(req) {
      const scope = req.scope === ArtifactScope.USER_GLOBAL ? ArtifactScope.USER_GLOBAL : ArtifactScope.REPO_LOCAL;
      const root = installPath(req.ideType, req.type, req.slug, scope);
      if (!root) throw new ConnectError("no install path for this tool, type and scope", Code.InvalidArgument);
      const home = scope === ArtifactScope.REPO_LOCAL ? "<checkout>" : req.ideType === IdeType.CODEX && req.type === ArtifactType.HOOK ? "~/.codex" : "~";
      if (req.fileRelativePaths.length === 0) return { files: [{ relativePath: root, isPrimary: true }], root: home };
      return { files: req.fileRelativePaths.map((r) => ({ relativePath: fileInstallPath(root, r), isPrimary: r === "SKILL.md" })), root: home };
    },
    listPersons() {
      return { persons: persons.map((p) => ({ personId: p.personId, displayName: p.displayName })) };
    },
    listTeams() {
      return { teams: teams.map((t) => ({ ...t, memberPersonIds: [...t.memberPersonIds] })) };
    },
    pauseEnforcement() {
      paused = true;
      return { paused };
    },
    getSettings() {
      return {
        enforcementPaused: paused,
        hooksEnabled,
        serverVersion: "mock",
        enforcementWindow: { seconds: BigInt(WINDOW_DAYS * 86_400) },
        maxEnforcementWindow: { seconds: BigInt(MAX_WINDOW_DAYS * 86_400) },
      };
    },
    resumeEnforcement() {
      paused = false;
      return { paused };
    },
  };

  return impl;
}

export const MOCK_REJECTED_TOKEN = "invalid";

export function createMockTransport(getToken: () => string | null, opts: MockOptions = {}): Transport {
  const impl = createMockState(opts);
  const latency = opts.latencyMs ?? 120;
  const auth: Interceptor = (next) => async (req) => {
    const header = req.header.get("Authorization") ?? "";
    if (!header.startsWith("Bearer ") || header === `Bearer ${MOCK_REJECTED_TOKEN}`)
      throw new ConnectError("admin token required", Code.Unauthenticated);
    if (latency) await new Promise((r) => setTimeout(r, latency));
    return next(req);
  };
  return createRouterTransport((router) => router.service(AdminService, impl), {
    transport: { interceptors: [authInterceptor(getToken)] },
    router: { interceptors: [auth] },
  });
}
