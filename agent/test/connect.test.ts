import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ConnectRouter, type HandlerContext } from "@connectrpc/connect";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { AgentService, type ReportStateRequest } from "@harness-control-plane/gen/harnessctl/v1/agent_pb";
import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { checkServerUrl, ConnectTransport } from "../src/transport/connect.js";
import { AGENT_VERSION } from "../src/version.js";

const HASH_A = "a".repeat(64);
const HASH_B = "b".repeat(64);
const UNTIL = new Date("2030-01-02T03:04:05.000Z");

/** A real Connect server for AgentService, so the JSON protocol and headers are exercised end to end. */
function startServer(): Promise<{ url: string; server: Server; auth: string[]; reports: ReportStateRequest[]; bodyCalls: string[][] }> {
  const auth: string[] = [];
  const reports: ReportStateRequest[] = [];
  const bodyCalls: string[][] = [];
  const seen = (ctx: HandlerContext) => auth.push(ctx.requestHeader.get("authorization") ?? "");
  const routes = (router: ConnectRouter) =>
    router.service(AgentService, {
      getEnforcedSet(_req, ctx) {
        seen(ctx);
        return {
          paused: false,
          artifacts: [
            {
              artifactId: "art-1",
              ideType: IdeType.CLAUDE_CODE,
              type: ArtifactType.SKILL,
              name: "Review",
              scope: ArtifactScope.REPO_LOCAL,
              repository: "example-org/example-repo",
              installPath: ".claude/skills/review/SKILL.md",
              primaryHash: HASH_A,
              files: [
                { relativePath: ".claude/skills/review/SKILL.md", contentHash: HASH_A, executable: false },
                { relativePath: ".claude/skills/review/run.sh", contentHash: HASH_B, executable: false },
              ],
              enforceUntil: timestampFromDate(UNTIL),
              expired: false,
            },
            {
              artifactId: "art-2",
              ideType: IdeType.CODEX,
              type: ArtifactType.HOOK,
              name: "Guard",
              scope: ArtifactScope.USER_GLOBAL,
              installPath: "hooks/guard.sh",
              primaryHash: HASH_B,
              files: [{ relativePath: "hooks/guard.sh", contentHash: HASH_B, executable: true }],
              hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 10 },
              hookCommand: '"${CODEX_HOME:-$HOME/.codex}"/hooks/guard.sh',
              expired: true,
            },
          ],
        };
      },
      getArtifactBodies(req, ctx) {
        seen(ctx);
        bodyCalls.push([...req.contentHashes]);
        if (req.contentHashes.includes("0".repeat(64))) throw new ConnectError("not in your set", Code.NotFound);
        if (req.contentHashes.includes("f".repeat(64))) throw new ConnectError("try later", Code.Unavailable);
        return { bodies: req.contentHashes.map((h) => ({ contentHash: h, content: new TextEncoder().encode(`body ${h[0]}`) })) };
      },
      reportState(req, ctx) {
        seen(ctx);
        reports.push(req);
        return {};
      },
    });
  const server = createServer(connectNodeAdapter({ routes }));
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolve({ url: `http://127.0.0.1:${port}/`, server, auth, reports, bodyCalls });
    });
  });
}

describe("ConnectTransport — harnessctl.v1.AgentService over Connect JSON", () => {
  let srv: Awaited<ReturnType<typeof startServer>>;
  let t: ConnectTransport;

  before(async () => {
    srv = await startServer();
    t = new ConnectTransport({ server: srv.url, token: "device-secret" });
  });

  after(() => new Promise<void>((resolve) => srv.server.close(() => resolve())));

  it("maps the enforced set into the engine's contract, enum values by proto name", async () => {
    const set = await t.getEnforcedSet();
    assert.equal(set.paused, false);
    assert.equal(set.artifacts.length, 2);
    const [skill, hook] = set.artifacts;
    assert.deepEqual(skill, {
      artifactId: "art-1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_SKILL",
      name: "Review",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      repoFullName: "example-org/example-repo",
      installPath: ".claude/skills/review/SKILL.md",
      primaryFileId: HASH_A,
      primaryHash: HASH_A,
      hookCommand: "",
      files: [
        { relativePath: ".claude/skills/review/SKILL.md", fileId: HASH_A, hash: HASH_A, executable: false },
        { relativePath: ".claude/skills/review/run.sh", fileId: HASH_B, hash: HASH_B, executable: false },
      ],
      expired: false,
      enforceUntil: UNTIL.toISOString(),
    });
    assert.equal(hook!.scope, "ARTIFACT_SCOPE_USER_GLOBAL");
    assert.equal(hook!.repoFullName, "");
    assert.equal(hook!.expired, true);
    assert.deepEqual(hook!.hookConfig, { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 10 });
    assert.equal(srv.auth.at(-1), "Bearer device-secret");
  });

  it("fetches bodies by content hash, inline", async () => {
    const refs = await t.getDownloads([HASH_A, HASH_B]);
    assert.deepEqual(srv.bodyCalls.at(-1), [HASH_A, HASH_B]);
    assert.deepEqual(
      refs.map((r) => [r.fileId, Buffer.from(r.bytes!).toString()]),
      [
        [HASH_A, "body a"],
        [HASH_B, "body b"],
      ],
    );
  });

  it("a permanent refusal is no bodies; a transient one throws", async () => {
    assert.deepEqual(await t.getDownloads(["0".repeat(64)]), []);
    await assert.rejects(t.getDownloads(["f".repeat(64)]), /GetArtifactBodies/);
  });

  it("reports the full state with enum numbers and every field", async () => {
    await t.report({
      deviceId: "dev-1",
      agentVersion: AGENT_VERSION,
      knownRepositories: ["example-org/example-repo"],
      installed: [
        { ide: "IDE_TYPE_CODEX", repo: "", checkout: "", sourcePath: "hooks/guard.sh", contentHash: HASH_B },
        { ide: "IDE_TYPE_CLAUDE_CODE", repo: "example-org/example-repo", checkout: "0123456789abcdef", sourcePath: "CLAUDE.md", contentHash: HASH_A },
      ],
      applyFailures: [{ artifactId: "art-1", repository: "example-org/example-repo", contentHash: HASH_A, reason: "write failed" }],
    });
    const r = srv.reports.at(-1)!;
    assert.equal(r.deviceId, "dev-1");
    assert.equal(r.agentVersion, AGENT_VERSION);
    assert.deepEqual(r.knownRepositories, ["example-org/example-repo"]);
    assert.equal(r.installed[0]!.ideType, IdeType.CODEX);
    assert.equal(r.installed[0]!.sourcePath, "hooks/guard.sh");
    assert.equal(r.installed[1]!.checkout, "0123456789abcdef");
    assert.equal(r.installed[1]!.repository, "example-org/example-repo");
    assert.equal(r.applyFailures[0]!.contentHash, HASH_A);
    assert.equal(r.applyFailures[0]!.reason, "write failed");
  });

  it("an unreachable server surfaces as an error", async () => {
    const bad = new ConnectTransport({ server: "http://127.0.0.1:1", token: "x", timeoutMs: 2000 });
    await assert.rejects(bad.getEnforcedSet());
  });

  it("requires a server and a token", () => {
    assert.throws(() => new ConnectTransport({ server: "", token: "t" }), /server URL/);
    assert.throws(() => new ConnectTransport({ server: "http://h", token: "" }), /token/);
  });
});

describe("AGENT_VERSION", () => {
  it("equals the package version", () => {
    const pkg = JSON.parse(readFileSync(fileURLToPath(new URL("../../package.json", import.meta.url)), "utf8")) as {
      version: string;
    };
    assert.equal(AGENT_VERSION, pkg.version);
  });
});

describe("checkServerUrl: the device token never crosses the network in clear text", () => {
  for (const url of ["https://hcp.example.com", "http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "http://127.1.2.3"]) {
    it(`accepts ${url}`, () => assert.doesNotThrow(() => checkServerUrl(url)));
  }

  it("refuses plain HTTP to another host, naming the opt-out", () => {
    assert.throws(() => checkServerUrl("http://hcp.example.com"), /plain HTTP to hcp\.example\.com.*--insecure-http/);
    assert.throws(() => checkServerUrl("http://10.0.0.5:8080"), /plain HTTP/);
    assert.throws(() => checkServerUrl("http://127.0.0.1.example.com"), /plain HTTP/, "a lookalike name is not loopback");
  });

  it("allows plain HTTP to another host only when asked to", () => {
    assert.doesNotThrow(() => checkServerUrl("http://hcp.example.com", true));
  });

  it("refuses other schemes and garbage", () => {
    assert.throws(() => checkServerUrl("ftp://hcp.example.com"), /https/);
    assert.throws(() => checkServerUrl("not a url"), /not a valid URL/);
  });

  it("the transport applies the same check", () => {
    assert.throws(() => new ConnectTransport({ server: "http://hcp.example.com", token: "t" }), /plain HTTP/);
    assert.doesNotThrow(() => new ConnectTransport({ server: "http://hcp.example.com", token: "t", allowInsecureHttp: true }));
  });
});
