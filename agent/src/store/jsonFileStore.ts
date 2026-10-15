import { mkdir, open, readFile, rename, rm, writeFile } from "node:fs/promises";
import { homedir, hostname } from "node:os";
import { join } from "node:path";
import { normalizeState, type AgentState } from "../engine/state.js";
import type { Store } from "../ports.js";

const STATE_FILE = "state.json";
const LOCK_FILE = "state.lock";
// Longer than a slow pass (a full body download), short enough that a wedged holder surfaces.
const DEFAULT_LOCK_TIMEOUT_MS = 120_000;
const LOCK_POLL_MS = 100;

export interface JsonFileStoreOptions {
  lockTimeoutMs?: number;
}

/** Whether the process recorded in a lock file can still be holding it. */
function holderAlive(raw: string): boolean {
  let holder: { pid?: unknown; host?: unknown };
  try {
    holder = JSON.parse(raw) as { pid?: unknown; host?: unknown };
  } catch {
    // Written but not yet filled, or truncated: treat as held; the timeout bounds the wait.
    return true;
  }
  if (typeof holder.pid !== "number" || holder.host !== hostname()) return true;
  try {
    process.kill(holder.pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === "EPERM";
  }
}

export function defaultStateDir(): string {
  const xdg = process.env["XDG_CONFIG_HOME"];
  return join(xdg && xdg !== "" ? xdg : join(homedir(), ".config"), "harness-agent");
}

/**
 * State as one JSON file under `dir`, written stage-then-rename so a crash leaves the previous
 * version intact. Owner-only permissions: the file records local paths and content previews.
 */
export class JsonFileStore implements Store {
  readonly path: string;
  readonly lockPath: string;
  private readonly lockTimeoutMs: number;

  constructor(
    private readonly dir: string = defaultStateDir(),
    opts: JsonFileStoreOptions = {},
  ) {
    this.path = join(dir, STATE_FILE);
    this.lockPath = join(dir, LOCK_FILE);
    this.lockTimeoutMs = opts.lockTimeoutMs ?? DEFAULT_LOCK_TIMEOUT_MS;
  }

  /**
   * Exclusive across processes sharing the directory (a manual `sync` and the service's `watch`),
   * held from load to save. A lock whose holder process is gone is taken over.
   */
  async lock(): Promise<() => Promise<void>> {
    await mkdir(this.dir, { recursive: true, mode: 0o700 });
    const deadline = Date.now() + this.lockTimeoutMs;
    for (;;) {
      try {
        const fh = await open(this.lockPath, "wx", 0o600);
        try {
          await fh.writeFile(JSON.stringify({ pid: process.pid, host: hostname(), at: new Date().toISOString() }));
        } finally {
          await fh.close();
        }
        return () => rm(this.lockPath, { force: true });
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw new Error(`locking ${this.lockPath}: ${(err as Error).message}`, { cause: err });
      }
      const raw = await readFile(this.lockPath, "utf8").catch(() => null);
      if (raw !== null && !holderAlive(raw)) {
        await rm(this.lockPath, { force: true });
        continue;
      }
      if (Date.now() >= deadline) {
        throw new Error(`state directory is locked by another agent process (${this.lockPath}${raw ? `: ${raw}` : ""})`);
      }
      await new Promise((r) => setTimeout(r, LOCK_POLL_MS));
    }
  }

  async load(): Promise<AgentState | null> {
    let raw: string;
    try {
      raw = await readFile(this.path, "utf8");
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return null;
      throw err;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      // A corrupt file is recovered as empty rather than wedging every later pass.
      return null;
    }
    return normalizeState(parsed);
  }

  async save(state: AgentState): Promise<void> {
    await mkdir(this.dir, { recursive: true, mode: 0o700 });
    const tmp = `${this.path}.tmp-${process.pid}-${Date.now()}`;
    await writeFile(tmp, `${JSON.stringify(state, null, 2)}\n`, { encoding: "utf8", mode: 0o600 });
    await rename(tmp, this.path);
  }
}
