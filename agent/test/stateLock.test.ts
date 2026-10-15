import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { createAgent } from "../src/agent.js";
import type { DownloadRef, EnforcedSet, StateReport } from "../src/contract.js";
import type { Transport } from "../src/ports.js";
import { JsonFileStore } from "../src/store/jsonFileStore.js";
import { testContext } from "./helpers.js";

/** Serves an empty set; `hold` parks the read until `release` is called. */
class GatedTransport implements Transport {
  reads = 0;
  private gate: Promise<void> | null = null;
  private open: (() => void) | null = null;
  readStarted: Promise<void>;
  private started!: () => void;

  constructor(hold: boolean) {
    this.readStarted = new Promise((r) => (this.started = r));
    if (hold) this.gate = new Promise((r) => (this.open = r));
  }

  release(): void {
    this.open?.();
  }

  async getEnforcedSet(): Promise<EnforcedSet> {
    this.reads++;
    this.started();
    if (this.gate !== null) await this.gate;
    return { artifacts: [] };
  }

  async getDownloads(): Promise<DownloadRef[]> {
    return [];
  }

  async report(_: StateReport): Promise<void> {}
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe("state-dir lock: a manual sync and the service's watch never interleave", () => {
  let dir: string;
  let stateDir: string;
  const agentOn = (transport: Transport, store = new JsonFileStore(stateDir)) =>
    createAgent({ transport, store, roots: { ...testContext(dir).paths, discoverProjects: false } });

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "state-lock-"));
    mkdirSync(join(dir, "home"), { recursive: true });
    stateDir = join(dir, "state");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("a second process's pass waits for the first to save, so neither save is lost", async () => {
    const slow = new GatedTransport(true);
    const fast = new GatedTransport(false);
    const first = agentOn(slow).sync();
    await slow.readStarted;

    const second = agentOn(fast).sync();
    await sleep(300);
    assert.equal(fast.reads, 0, "the second pass has not started while the first holds the lock");

    slow.release();
    await Promise.all([first, second]);
    const saved = await new JsonFileStore(stateDir).load();
    assert.equal(saved?.history.length, 2, "both passes recorded");
    assert.equal(existsSync(join(stateDir, "state.lock")), false, "released");
  });

  it("a lock left by a process that is gone is taken over", async () => {
    mkdirSync(stateDir, { recursive: true });
    writeFileSync(join(stateDir, "state.lock"), JSON.stringify({ pid: 2 ** 22 + 12345, host: hostname() }));
    const result = await agentOn(new GatedTransport(false)).sync();
    assert.equal(result.fetched, true);
  });

  it("a lock held by a live process times out with a clear error", async () => {
    mkdirSync(stateDir, { recursive: true });
    writeFileSync(join(stateDir, "state.lock"), JSON.stringify({ pid: process.pid, host: hostname() }));
    const store = new JsonFileStore(stateDir, { lockTimeoutMs: 300 });
    await assert.rejects(agentOn(new GatedTransport(false), store).sync(), /state directory is locked/);
  });
});
