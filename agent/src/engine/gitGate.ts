import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { anchoredOperation, readAnchoredFile } from "./anchoredFs.js";
import { writeAtomic } from "./fsops.js";
import { dirname, isAbsolute, resolve } from "node:path";
import { promisify } from "node:util";
import { SCOPE_REPO_LOCAL, type EnforcedArtifact } from "../contract.js";
import type { Logger } from "../ports.js";
import { activationScopeOf, isHookArtifact, resolveArtifactFiles, type TargetedArtifact } from "./artifacts.js";
import { noopLogger } from "./context.js";
import { resolveInstallPath } from "./fsops.js";
import { activationRelPath, ideSupportsHookActivation } from "./hookActivation.js";
import type { GitExcludeRecord } from "./state.js";

const execFileAsync = promisify(execFile);

const GIT_TIMEOUT_MS = 5000;

// Far below the smallest real ARG_MAX (~256 KiB on macOS), leaving room for env and fixed args.
const GIT_PATHSPEC_BATCH_BYTES = 64 * 1024;

/**
 * Repository-local git variables (`git rev-parse --local-env-vars`). Each points git at a different
 * repository than `-C <root>`; a stale GIT_INDEX_FILE makes `ls-files` silently answer "untracked".
 */
const GIT_LOCAL_ENV_VARS = [
  "GIT_ALTERNATE_OBJECT_DIRECTORIES",
  "GIT_CONFIG",
  "GIT_CONFIG_PARAMETERS",
  "GIT_CONFIG_COUNT",
  "GIT_OBJECT_DIRECTORY",
  "GIT_DIR",
  "GIT_WORK_TREE",
  "GIT_IMPLICIT_WORK_TREE",
  "GIT_GRAFT_FILE",
  "GIT_INDEX_FILE",
  "GIT_NO_REPLACE_OBJECTS",
  "GIT_REPLACE_REF_BASE",
  "GIT_PREFIX",
  "GIT_SHALLOW_FILE",
  "GIT_COMMON_DIR",
];

/**
 * Process env minus the repository-local variables, with LC_ALL=C so stderr classification is
 * locale-stable. Config variables are kept: the probe must see the ignore rules the engineer's git sees.
 */
export function gitProbeEnv(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, LC_ALL: "C" };
  for (const key of GIT_LOCAL_ENV_VARS) delete env[key];
  return env;
}

/** Split into argv-safe batches: thousands of paths in one argv fails E2BIG, which is fail-closed here. */
export function batchPathspecs(relPaths: string[], maxBytes = GIT_PATHSPEC_BATCH_BYTES): string[][] {
  const batches: string[][] = [];
  let current: string[] = [];
  let bytes = 0;
  for (const path of relPaths) {
    const size = Buffer.byteLength(path, "utf8") + 1; // +1 for the argv NUL
    if (current.length > 0 && bytes + size > maxBytes) {
      batches.push(current);
      current = [];
      bytes = 0;
    }
    current.push(path);
    bytes += size;
  }
  if (current.length > 0) batches.push(current);
  return batches;
}

/** promisify(execFile) reports the exit status as `code`; the classifiers read `status`. */
function asSyncShapedError(err: unknown): unknown {
  const e = err as { code?: unknown; status?: unknown; killed?: boolean; stderr?: string | Buffer } | null;
  if (e === null || typeof e !== "object") return err;
  if (typeof e.status === "number") return err;
  return { status: typeof e.code === "number" ? e.code : null, killed: e.killed === true, stderr: e.stderr };
}

function gitStderr(err: unknown): string {
  const s = (err as { stderr?: string | Buffer } | null)?.stderr;
  return typeof s === "string" ? s : (s?.toString("utf8") ?? "");
}

/**
 * True only for a confirmed non-git directory: exit 128 with the not-a-repo diagnostic. Git also exits
 * 128 for operational failures (dubious ownership, bad GIT_DIR, corrupt metadata), which must not
 * take the quiet path.
 */
export function isNonGitDirError(err: unknown): boolean {
  const e = err as { status?: number | null; killed?: boolean } | null;
  if (e == null || e.status !== 128 || e.killed === true) return false;
  const stderr = gitStderr(err);
  return /not a git repository/i.test(stderr) || /not a working tree/i.test(stderr);
}

/**
 * Append `relPaths` to one checkout's git exclude file, then confirm git actually ignores them.
 * Resolved via `--git-path` so a linked worktree (whose `.git` is a file) shares its checkout's file.
 * Only hides untracked paths; callers pass the untracked subset. A genuine non-git dir is safe (true);
 * any operational failure is logged and returns false so the caller does not write.
 */
export async function ensureGitExcluded(
  root: string,
  relPaths: string[],
  log: Logger = noopLogger,
  onAppended?: (excludeFile: string, appended: string[]) => void,
): Promise<boolean> {
  if (relPaths.length === 0) return true;
  let excludePath: string;
  try {
    const { stdout } = await execFileAsync("git", ["-C", root, "rev-parse", "--git-path", "info/exclude"], {
      encoding: "utf8",
      timeout: GIT_TIMEOUT_MS,
      env: gitProbeEnv(),
    });
    const out = stdout.trim();
    if (out === "") {
      log.warn("git_exclude_unresolved", { root, error: "git returned an empty exclude path" });
      return false;
    }
    excludePath = isAbsolute(out) ? out : resolve(root, out);
  } catch (err) {
    if (isNonGitDirError(asSyncShapedError(err))) return true;
    log.warn("git_exclude_unresolved", {
      root,
      error: err instanceof Error ? err.message : String(err),
      stderr: gitStderr(err).slice(0, 200),
    });
    return false;
  }
  try {
    // Git worktrees may share a common Git directory outside this checkout. Trust that Git-selected
    // directory as the root, while confining its info/exclude descendants.
    const gitRoot = dirname(dirname(excludePath));
    let current = "";
    try {
      current = readAnchoredFile(gitRoot, excludePath).toString("utf8");
    } catch {
      current = "";
    }
    const present = new Set(current.split("\n").map((l) => l.trim()));
    const missing = Array.from(new Set(relPaths)).filter((l) => !present.has(l));
    if (missing.length > 0) {
      const prefix = current !== "" && !current.endsWith("\n") ? "\n" : "";
      anchoredOperation("append", gitRoot, excludePath, { data: Buffer.from(`${prefix}${missing.join("\n")}\n`) });
    }
    onAppended?.(excludePath, missing);
  } catch (err) {
    log.warn("git_exclude_write_failed", { root, error: err instanceof Error ? err.message : String(err) });
    return false;
  }
  return gitIgnoresAll(root, relPaths, log);
}

/**
 * Whether git ignores every path. An exclude line is not proof: a committed `.gitignore` negation
 * outranks it. `--no-index` asks about rules only. Exit 1 is a clean "none ignored"; anything else
 * git cannot answer is false.
 */
async function gitIgnoresAll(root: string, relPaths: string[], log: Logger): Promise<boolean> {
  if (relPaths.length === 0) return true;
  // Each batch needs a positive answer per path, so an unanswerable batch is false, not a skip.
  for (const batch of batchPathspecs(Array.from(new Set(relPaths)))) {
    let ignored: Set<string>;
    try {
      // check-ignore rejects --literal-pathspecs, unlike ls-files.
      const { stdout } = await execFileAsync("git", ["-C", root, "check-ignore", "--no-index", "--", ...batch], {
        encoding: "utf8",
        timeout: GIT_TIMEOUT_MS,
        env: gitProbeEnv(),
      });
      ignored = new Set(stdout.split("\n").map((l) => l.trim()).filter((l) => l !== ""));
    } catch (err) {
      const shaped = asSyncShapedError(err) as { status?: number | null };
      if (isNonGitDirError(shaped)) return true;
      if (shaped.status === 1) {
        ignored = new Set();
      } else {
        log.warn("git_ignore_probe_failed", {
          root,
          paths: batch.length,
          error: err instanceof Error ? err.message : String(err),
          stderr: gitStderr(err).slice(0, 200),
        });
        return false;
      }
    }
    const visible = batch.filter((path) => !ignored.has(path));
    if (visible.length > 0) {
      log.warn("git_exclude_ineffective", { root, paths: visible.slice(0, 5) });
      return false;
    }
  }
  return true;
}

/**
 * The subset of `relPaths` git tracks under `root`, or null when git could not answer (fail closed).
 * A genuine non-git dir tracks nothing. `-z` so no path is quoted by core.quotePath.
 */
export async function gitTrackedSubset(
  root: string,
  relPaths: string[],
  log: Logger = noopLogger,
  modifiedOnly = false,
): Promise<Set<string> | null> {
  if (relPaths.length === 0) return new Set();
  const tracked = new Set<string>();
  // One failed batch fails the whole probe: a partial answer must never read as "the rest are untracked".
  for (const batch of batchPathspecs(relPaths)) {
    try {
      // --literal-pathspecs is global (precedes the subcommand): an install path is never a glob.
      const args = ["--literal-pathspecs", "-C", root, "ls-files", "-z", ...(modifiedOnly ? ["--modified"] : []), "--", ...batch];
      const { stdout } = await execFileAsync("git", args, {
        encoding: "utf8",
        timeout: GIT_TIMEOUT_MS,
        env: gitProbeEnv(),
      });
      for (const path of stdout.split("\0")) if (path !== "") tracked.add(path);
    } catch (err) {
      if (isNonGitDirError(asSyncShapedError(err))) return new Set();
      log.warn("git_tracked_probe_failed", {
        root,
        paths: batch.length,
        error: err instanceof Error ? err.message : String(err),
        stderr: gitStderr(err).slice(0, 200),
      });
      return null;
    }
  }
  return tracked;
}

/**
 * One checkout's write gate: whether it could be established, which paths git already tracks, and
 * which of those have a working copy matching git's index, so git holds the bytes a write replaces.
 */
export interface RootWriteGate {
  established: boolean;
  trackedPaths: ReadonlySet<string>;
  cleanTrackedPaths?: ReadonlySet<string>;
}

export type GateVerdict = { kind: "write" } | { kind: "defer" };

/**
 * Every root-relative path a target write adds content to, the hook activation file included. An
 * inactive hook's activation step only removes our entry, so it is not gated — gating it would strand
 * a disabled hook's entry live in a checkout whose activation file is tracked.
 */
export function writePathsFor(artifact: EnforcedArtifact): string[] {
  const paths = resolveArtifactFiles(artifact).map((f) => f.installPath);
  if (isHookArtifact(artifact) && ideSupportsHookActivation(artifact.ideType) && artifact.inactive !== true) {
    const rel = activationRelPath(artifact.ideType, artifact.installPath, activationScopeOf(artifact.scope));
    if (rel !== null) paths.push(rel);
  }
  return paths;
}

/**
 * Establish the write gate for every repo-local checkout this pass writes into. An untracked path is
 * excluded before any body is written — a file written first is committable in the window between.
 * A tracked path takes no exclude line (inert for tracked paths) and is written in place. User-global
 * writes land outside any shared repo and are not gated.
 */
export async function buildWriteGates(
  targeted: TargetedArtifact[],
  log: Logger = noopLogger,
  excludes?: GitExcludeRecord[],
): Promise<Map<string, RootWriteGate>> {
  const pathsByRoot = new Map<string, Set<string>>();
  for (const { artifact, target } of targeted) {
    if (artifact.scope !== SCOPE_REPO_LOCAL) continue;
    // An expired artifact takes no exclude line and must not leave its checkout's gate unestablished.
    if (artifact.expired === true) continue;
    let set = pathsByRoot.get(target.root);
    if (set === undefined) {
      set = new Set();
      pathsByRoot.set(target.root, set);
    }
    for (const path of writePathsFor(artifact)) set.add(path);
  }

  const gates = new Map<string, RootWriteGate>();
  for (const [root, set] of pathsByRoot) {
    const paths = Array.from(set).sort();
    const tracked = await gitTrackedSubset(root, paths, log);
    if (tracked === null) {
      // Which paths need excluding is exactly what git failed to say: defer the whole root.
      gates.set(root, { established: false, trackedPaths: new Set() });
      continue;
    }
    const untracked = paths.filter((path) => !tracked.has(path));
    const excluded = await ensureGitExcluded(root, untracked, log, (file, appended) => {
      if (excludes !== undefined) recordExcludeLines(excludes, file, root, appended);
    });
    if (!excluded) log.warn("write_gate_unestablished_unexcluded", { root });
    // Fail closed: when git cannot say which tracked paths carry local edits, none counts as clean.
    const modified = await gitTrackedSubset(root, Array.from(tracked).sort(), log, true);
    const clean = modified === null ? new Set<string>() : new Set(Array.from(tracked).filter((p) => !modified.has(p)));
    gates.set(root, { established: excluded, trackedPaths: tracked, cleanTrackedPaths: clean });
  }
  return gates;
}

/** A root with no gate entry is ungated (user-global, or a root that contributed no path). */
export function gateVerdict(gates: Map<string, RootWriteGate>, t: TargetedArtifact): GateVerdict {
  if (t.artifact.scope !== SCOPE_REPO_LOCAL) return { kind: "write" };
  const gate = gates.get(t.target.root);
  if (gate === undefined) return { kind: "write" };
  return gate.established ? { kind: "write" } : { kind: "defer" };
}

/** Note the lines appended for `root`, and that `root` shares `file` even when it appended none. */
export function recordExcludeLines(records: GitExcludeRecord[], file: string, root: string, appended: string[]): void {
  let rec = records.find((r) => r.file === file);
  if (rec === undefined) {
    if (appended.length === 0) return;
    rec = { file, roots: [], lines: [] };
    records.push(rec);
  }
  if (!rec.roots.includes(root)) rec.roots.push(root);
  for (const line of appended) if (!rec.lines.includes(line)) rec.lines.push(line);
}

/**
 * Remove the exclude lines this agent appended whose path nothing still needs: no live target in any
 * checkout sharing the file will write it, and it is not on disk there (a file left in place because
 * it was edited keeps its line). Only the last matching line goes, so an engineer's duplicate stays.
 */
export function releaseGitExcludes(
  records: GitExcludeRecord[],
  claimedByRoot: ReadonlyMap<string, ReadonlySet<string>>,
  log: Logger = noopLogger,
): void {
  const onDisk = (root: string, rel: string): boolean => {
    const abs = resolveInstallPath(root, rel);
    return abs !== null && existsSync(abs);
  };
  for (const rec of records) {
    const release = new Set(
      rec.lines.filter((line) => !rec.roots.some((root) => claimedByRoot.get(root)?.has(line) === true || onDisk(root, line))),
    );
    if (release.size === 0) continue;
    if (existsSync(rec.file)) {
      try {
        const lines = readAnchoredFile(dirname(dirname(rec.file)), rec.file).toString("utf8").split("\n");
        for (const line of release) {
          for (let at = lines.length - 1; at >= 0; at--) {
            if (lines[at]!.trim() !== line) continue;
            lines.splice(at, 1);
            break;
          }
        }
        writeAtomic(rec.file, Buffer.from(lines.join("\n")), { root: dirname(dirname(rec.file)) });
      } catch (err) {
        log.warn("git_exclude_release_failed", { file: rec.file, error: err instanceof Error ? err.message : String(err) });
        continue;
      }
    }
    rec.lines = rec.lines.filter((line) => !release.has(line));
  }
  const kept = records.filter((r) => r.lines.length > 0);
  records.splice(0, records.length, ...kept);
}
