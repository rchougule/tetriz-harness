import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { emptyState } from "../src/engine/state.js";
import { JsonFileStore, defaultStateDir } from "../src/store/jsonFileStore.js";
import { MemoryStore } from "../src/store/memoryStore.js";

describe("JsonFileStore", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "store-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("an empty directory loads as null", async () => {
    assert.equal(await new JsonFileStore(join(dir, "state")).load(), null);
  });

  it("round-trips state, creating the directory owner-only", async () => {
    const store = new JsonFileStore(join(dir, "state"));
    const state = emptyState();
    state.consecutiveEmptyReads = 1;
    state.deviceId = "d-9";
    await store.save(state);

    assert.deepEqual(await store.load(), state);
    assert.equal(statSync(join(dir, "state")).mode & 0o777, 0o700);
    assert.equal(statSync(store.path).mode & 0o777, 0o600);
    assert.deepEqual(readdirSync(join(dir, "state")), ["state.json"], "no temp file left behind");
  });

  it("a corrupt file recovers as empty rather than wedging every pass", async () => {
    const store = new JsonFileStore(dir);
    writeFileSync(store.path, "{ not json");
    assert.equal(await store.load(), null);
  });

  it("a malformed field falls back to its empty value, the rest survive", async () => {
    const store = new JsonFileStore(dir);
    writeFileSync(store.path, JSON.stringify({ snapshot: "nope", consecutiveEmptyReads: 1, deviceId: "d" }));
    const loaded = await store.load();
    assert.deepEqual(loaded?.snapshot, []);
    assert.equal(loaded?.consecutiveEmptyReads, 1);
    assert.equal(loaded?.deviceId, "d");
  });

  it("the default directory honors XDG_CONFIG_HOME", () => {
    const saved = process.env["XDG_CONFIG_HOME"];
    try {
      process.env["XDG_CONFIG_HOME"] = "/tmp/xdg";
      assert.equal(defaultStateDir(), "/tmp/xdg/harness-agent");
      delete process.env["XDG_CONFIG_HOME"];
      assert.match(defaultStateDir(), /\.config\/harness-agent$/);
    } finally {
      if (saved === undefined) delete process.env["XDG_CONFIG_HOME"];
      else process.env["XDG_CONFIG_HOME"] = saved;
    }
  });

  it("writes pretty JSON so the file is inspectable", async () => {
    const store = new JsonFileStore(dir);
    await store.save(emptyState());
    assert.match(readFileSync(store.path, "utf8"), /\n {2}"snapshot": \[\]/);
  });
});

describe("MemoryStore", () => {
  it("copies in and out, so a caller cannot alias its contents", async () => {
    const store = new MemoryStore();
    const state = emptyState();
    await store.save(state);
    state.deviceId = "mutated";
    const loaded = await store.load();
    assert.equal(loaded?.deviceId, undefined);
    loaded!.deviceId = "mutated again";
    assert.equal((await store.load())?.deviceId, undefined);
  });
});
