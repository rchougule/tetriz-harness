import { create, type DescEnum } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient, type Client, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";
import {
  AgentService,
  InstalledArtifactSchema,
  ApplyFailureSchema,
  ReportStateRequestSchema,
  type EnforcedArtifact as WireArtifact,
  type ReportStateRequest,
} from "@harness-control-plane/gen/harnessctl/v1/agent_pb";
import { ArtifactScopeSchema, ArtifactTypeSchema, IdeTypeSchema } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import type { DownloadRef, EnforcedArtifact, EnforcedSet, StateReport } from "../contract.js";
import { noopLogger } from "../engine/context.js";
import type { Logger, Transport } from "../ports.js";

// The single mapping between the engine's contract and harnessctl.v1.AgentService, spoken as the
// Connect protocol with JSON bodies.

const REQUEST_TIMEOUT_MS = 30_000;

export interface ConnectTransportOptions {
  server: string;
  token: string;
  logger?: Logger;
  timeoutMs?: number;
  /** Allow plain `http://` to a host other than this machine. The device token then crosses the network unencrypted. */
  allowInsecureHttp?: boolean;
}

/** This machine: the hosts a plain-HTTP token never leaves. */
function isLoopbackHost(hostname: string): boolean {
  const h = hostname.replace(/^\[|\]$/g, "").toLowerCase();
  return h === "localhost" || h.endsWith(".localhost") || h === "::1" || /^127(\.\d{1,3}){3}$/.test(h);
}

/**
 * Refuse a server URL that would send the device token in clear text: `https://` anywhere, `http://`
 * only to this machine unless `allowInsecureHttp` says the network in between is trusted.
 */
export function checkServerUrl(server: string, allowInsecureHttp = false): void {
  let url: URL;
  try {
    url = new URL(server);
  } catch {
    throw new Error(`server URL is not a valid URL: ${server}`);
  }
  if (url.protocol === "https:") return;
  if (url.protocol !== "http:") throw new Error(`server URL must use https:// (or http:// to this machine): ${server}`);
  if (isLoopbackHost(url.hostname) || allowInsecureHttp) return;
  throw new Error(
    `refusing to send the device token over plain HTTP to ${url.host}: use https://, or pass --insecure-http on a network you trust`,
  );
}

/** The proto value name of `n` ("IDE_TYPE_CODEX"); an unknown number keeps its number so it matches nothing. */
function enumName(desc: DescEnum, n: number): string {
  return desc.value[n]?.name ?? `${desc.typeName}(${n})`;
}

function enumNumber(desc: DescEnum, name: string): number {
  const v = desc.values.find((e) => e.name === name);
  if (v === undefined) throw new Error(`unknown ${desc.typeName} value ${name}`);
  return v.number;
}

export function fromWireArtifact(w: WireArtifact): EnforcedArtifact {
  const files = w.files.map((f) => ({
    relativePath: f.relativePath,
    fileId: f.contentHash,
    hash: f.contentHash,
    executable: f.executable,
  }));
  const a: EnforcedArtifact = {
    artifactId: w.artifactId,
    ideType: enumName(IdeTypeSchema, w.ideType),
    type: enumName(ArtifactTypeSchema, w.type),
    name: w.name,
    scope: enumName(ArtifactScopeSchema, w.scope),
    repoFullName: w.repository,
    installPath: w.installPath,
    primaryFileId: w.primaryHash,
    primaryHash: w.primaryHash,
    hookCommand: w.hookCommand,
    files,
    expired: w.expired,
  };
  if (w.hookConfig !== undefined) {
    a.hookConfig = {
      event: w.hookConfig.event,
      matcher: w.hookConfig.matcher,
      timeoutSeconds: w.hookConfig.timeoutSeconds,
    };
  }
  if (w.enforceUntil !== undefined) a.enforceUntil = timestampDate(w.enforceUntil).toISOString();
  return a;
}

export function toWireReport(r: StateReport): ReportStateRequest {
  return create(ReportStateRequestSchema, {
    deviceId: r.deviceId,
    agentVersion: r.agentVersion,
    knownRepositories: r.knownRepositories,
    installed: r.installed.map((i) =>
      create(InstalledArtifactSchema, {
        ideType: enumNumber(IdeTypeSchema, i.ide),
        repository: i.repo,
        checkout: i.checkout,
        sourcePath: i.sourcePath,
        contentHash: i.contentHash,
      }),
    ),
    applyFailures: r.applyFailures.map((f) =>
      create(ApplyFailureSchema, {
        artifactId: f.artifactId,
        repository: f.repository,
        contentHash: f.contentHash,
        reason: f.reason,
      }),
    ),
  });
}

/** Codes that are a verdict on the request itself, so retrying the same batch cannot help. */
const PERMANENT_BODY_CODES = new Set([Code.InvalidArgument, Code.NotFound, Code.PermissionDenied, Code.FailedPrecondition]);

export class ConnectTransport implements Transport {
  private readonly client: Client<typeof AgentService>;
  private readonly log: Logger;
  private readonly timeoutMs: number;

  constructor(opts: ConnectTransportOptions) {
    if (!opts.server) throw new Error("server URL is required");
    if (!opts.token) throw new Error("device token is required");
    checkServerUrl(opts.server, opts.allowInsecureHttp === true);
    this.log = opts.logger ?? noopLogger;
    this.timeoutMs = opts.timeoutMs ?? REQUEST_TIMEOUT_MS;
    const auth: Interceptor = (next) => (req) => {
      req.header.set("Authorization", `Bearer ${opts.token}`);
      return next(req);
    };
    const rpc = createConnectTransport({
      baseUrl: opts.server.replace(/\/+$/, ""),
      httpVersion: "1.1",
      useBinaryFormat: false,
      interceptors: [auth],
    });
    this.client = createClient(AgentService, rpc);
  }

  async getEnforcedSet(): Promise<EnforcedSet> {
    const res = await this.client.getEnforcedSet({}, { timeoutMs: this.timeoutMs });
    return { artifacts: res.artifacts.map(fromWireArtifact), paused: res.paused };
  }

  /** A permanent refusal answers "no bodies for this batch" (per-file failures); a transient error throws. */
  async getDownloads(fileIds: string[]): Promise<DownloadRef[]> {
    try {
      const res = await this.client.getArtifactBodies({ contentHashes: fileIds }, { timeoutMs: this.timeoutMs });
      return res.bodies.map((b) => ({ fileId: b.contentHash, bytes: b.content }));
    } catch (err) {
      const ce = ConnectError.from(err);
      if (PERMANENT_BODY_CODES.has(ce.code)) {
        this.log.warn("bodies_rejected", { code: Code[ce.code], message: ce.rawMessage.slice(0, 300) });
        return [];
      }
      throw new Error(`GetArtifactBodies: ${ce.message}`, { cause: err });
    }
  }

  async report(state: StateReport): Promise<void> {
    await this.client.reportState(toWireReport(state), { timeoutMs: this.timeoutMs });
  }
}
