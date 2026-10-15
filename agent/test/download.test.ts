import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { DownloadRef } from "../src/contract.js";
import { noopLogger } from "../src/engine/context.js";
import { downloadArtifactBodies, type FetchImpl } from "../src/engine/download.js";
import type { Logger, LogFields } from "../src/ports.js";
import { sha256 } from "./helpers.js";
import { SIGNATURE_ERROR, assertNoCredentialMaterial } from "./s3Fixtures.js";

function transportOf(refs: (ids: string[]) => DownloadRef[] | Promise<DownloadRef[]>) {
  const calls: string[][] = [];
  return {
    calls,
    getDownloads: async (ids: string[]) => {
      calls.push(ids);
      return refs(ids);
    },
  };
}

const neverFetch: FetchImpl = async () => {
  throw new Error("fetch must not be called");
};

describe("downloadArtifactBodies — verify-before-write", () => {
  it("returns an empty result for an empty request, without calling the transport", async () => {
    const t = transportOf(() => []);
    const result = await downloadArtifactBodies(new Map(), t, noopLogger, neverFetch);
    assert.deepEqual(result.bodies, new Map());
    assert.deepEqual(result.failed, new Map());
    assert.equal(t.calls.length, 0);
  });

  it("downloads and verifies a body matching its expected hash", async () => {
    const content = "# CLAUDE.md\nbody\n";
    const t = transportOf(() => [{ fileId: "f1", url: "https://blob.example.com/f1?sig=abc" }]);
    const fetchImpl: FetchImpl = async (url) => {
      assert.equal(url, "https://blob.example.com/f1?sig=abc");
      return new Response(content);
    };

    const result = await downloadArtifactBodies(new Map([["f1", sha256(content)]]), t, noopLogger, fetchImpl);
    assert.equal(result.bodies.get("f1")?.toString("utf8"), content);
    assert.equal(result.failed.size, 0);
  });

  it("accepts inline bytes, verified the same way", async () => {
    const content = "inline body\n";
    const t = transportOf(() => [{ fileId: "f1", bytes: new TextEncoder().encode(content) }]);
    const result = await downloadArtifactBodies(new Map([["f1", sha256(content)]]), t, noopLogger, neverFetch);
    assert.equal(result.bodies.get("f1")?.toString("utf8"), content);
  });

  it("inline bytes that fail verification are a 'hash' failure, never returned", async () => {
    const t = transportOf(() => [{ fileId: "f1", bytes: new TextEncoder().encode("tampered") }]);
    const result = await downloadArtifactBodies(new Map([["f1", sha256("expected")]]), t, noopLogger, neverFetch);
    assert.equal(result.bodies.has("f1"), false);
    assert.equal(result.failed.get("f1"), "hash");
  });

  it("classifies a hash-verification failure as 'hash' (a genuine integrity violation) — never returned for writing", async () => {
    const t = transportOf(() => [{ fileId: "f1", url: "https://blob.example.com/f1?sig=abc" }]);
    const fetchImpl: FetchImpl = async () => new Response("tampered or stale content");

    const result = await downloadArtifactBodies(new Map([["f1", sha256("something else entirely")]]), t, noopLogger, fetchImpl);
    assert.equal(result.bodies.has("f1"), false);
    assert.equal(result.failed.get("f1"), "hash");
  });

  it("classifies a non-2xx GET as a transient 'download' failure", async () => {
    const t = transportOf(() => [{ fileId: "f1", url: "https://blob.example.com/f1?sig=abc" }]);
    const fetchImpl: FetchImpl = async () => new Response("expired", { status: 403 });

    const result = await downloadArtifactBodies(new Map([["f1", sha256("x")]]), t, noopLogger, fetchImpl);
    assert.equal(result.bodies.has("f1"), false);
    assert.equal(result.failed.get("f1"), "download");
  });

  it("fails the file id as 'download' when the server issues no ref for it", async () => {
    const t = transportOf(() => []);
    const result = await downloadArtifactBodies(new Map([["f1", sha256("x")]]), t, noopLogger, neverFetch);
    assert.equal(result.bodies.has("f1"), false);
    assert.equal(result.failed.get("f1"), "download");
  });

  it("propagates a transport throw (the pass aborts and retries next time)", async () => {
    const t = transportOf(() => {
      throw new Error("rate limited");
    });
    await assert.rejects(() => downloadArtifactBodies(new Map([["f1", sha256("x")]]), t, noopLogger, neverFetch));
  });

  it("batches file ids to the per-request cap", async () => {
    const wanted = new Map(Array.from({ length: 250 }, (_, i) => [`f${i}`, sha256("x")] as const));
    const t = transportOf(() => []);
    await downloadArtifactBodies(wanted, t, noopLogger, neverFetch);
    assert.deepEqual(t.calls.map((c) => c.length), [100, 100, 50]);
  });
});

// Guards the emitted line, not the helper: a call site can regress with the helper's tests green.
describe("body_get_failed log line carries no credential material", () => {
  it("logs the diagnosis but none of the signing material", async () => {
    const SIGNED_URL =
      "https://s3.us-east-1.amazonaws.com/b/k?X-Amz-Credential=ASIALEAKKEY%2F20260803%2Fus-east-1%2Fs3%2Faws4_request" +
      "&X-Amz-Security-Token=FwoGZXIvYXdzEXAMPLETOKEN&X-Amz-Signature=deadbeef";
    const lines: string[] = [];
    const capture = (event: string, fields?: LogFields) => lines.push(JSON.stringify({ event, ...fields }));
    const log: Logger = { debug: capture, info: capture, warn: capture, error: capture };
    const t = transportOf(() => [{ fileId: "f1", url: SIGNED_URL }]);
    const fetchImpl: FetchImpl = async () => new Response(SIGNATURE_ERROR, { status: 403 });

    await downloadArtifactBodies(new Map([["f1", sha256("x")]]), t, log, fetchImpl);

    const failed = lines.filter((l) => l.includes("body_get_failed"));
    // Without this, a line that never got written would pass every assertion below.
    assert.equal(failed.length, 1, `expected exactly one body_get_failed line, got ${failed.length}`);
    assertNoCredentialMaterial(failed[0]!, assert.ok);
    // ...and still names the cause, or the redaction has made the field useless.
    assert.match(failed[0]!, /SignatureDoesNotMatch/);
  });
});
