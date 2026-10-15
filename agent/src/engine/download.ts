import { createHash } from "node:crypto";
import { DOWNLOAD_BATCH_MAX, type DownloadRef } from "../contract.js";
import type { Logger, Transport } from "../ports.js";
import { maskPresignedUrl, redactUrlIn, s3ErrorSummary } from "./redact.js";

// Every body handed back has already been sha256-verified against its expected hash; a mismatch is
// reported and never returned to be written. This is the one place that holds verify-before-write.

const GET_TIMEOUT_MS = 90_000;

/**
 * "download" is transient (not issued, network, non-2xx); "hash" is a genuine integrity violation.
 * They are reported with different wording because the server alerts on the latter only.
 */
export type DownloadFailureKind = "download" | "hash";

type VerifyOutcome = { bytes: Buffer } | { failure: DownloadFailureKind };

export type FetchImpl = (url: string, init?: RequestInit) => Promise<Response>;

function sha256(bytes: Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

async function fetchAndVerify(
  url: string,
  fileId: string,
  expectedHash: string,
  log: Logger,
  fetchImpl: FetchImpl,
): Promise<VerifyOutcome> {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), GET_TIMEOUT_MS);
  try {
    const response = await fetchImpl(url, { signal: controller.signal });
    if (!response.ok) {
      // A 403 can be access, key policy, or an expired signature; only the body's code tells them apart.
      const detail = s3ErrorSummary(await response.text().catch(() => ""));
      log.warn("body_get_failed", { url: maskPresignedUrl(url), fileId, status: response.status, detail });
      return { failure: "download" };
    }
    const bytes = Buffer.from(await response.arrayBuffer());
    if (sha256(bytes) !== expectedHash) {
      log.warn("hash_mismatch", { url: maskPresignedUrl(url), fileId });
      return { failure: "hash" };
    }
    return { bytes };
  } catch (err) {
    log.warn("body_get_error", {
      url: maskPresignedUrl(url),
      fileId,
      error: redactUrlIn(err instanceof Error ? err.message : String(err), url),
    });
    return { failure: "download" };
  } finally {
    clearTimeout(timeoutId);
  }
}

async function verifyRef(
  ref: DownloadRef,
  expectedHash: string,
  log: Logger,
  fetchImpl: FetchImpl,
): Promise<VerifyOutcome> {
  if (ref.bytes !== undefined) {
    const bytes = Buffer.from(ref.bytes);
    if (sha256(bytes) !== expectedHash) {
      log.warn("hash_mismatch", { fileId: ref.fileId, inline: true });
      return { failure: "hash" };
    }
    return { bytes };
  }
  if (ref.url) return fetchAndVerify(ref.url, ref.fileId, expectedHash, log, fetchImpl);
  return { failure: "download" };
}

export interface DownloadResult {
  /** file_id → verified bytes. */
  bodies: Map<string, Buffer>;
  /** file_id → how it failed this pass. */
  failed: Map<string, DownloadFailureKind>;
}

/**
 * Resolve, download and verify every file id in `wanted` (file_id → expected sha256), batched to
 * the server's per-request cap. A transport throw propagates and aborts the pass.
 */
export async function downloadArtifactBodies(
  wanted: Map<string, string>,
  transport: Pick<Transport, "getDownloads">,
  log: Logger,
  fetchImpl: FetchImpl = (url, init) => fetch(url, init),
): Promise<DownloadResult> {
  const bodies = new Map<string, Buffer>();
  const failed = new Map<string, DownloadFailureKind>();
  if (wanted.size === 0) return { bodies, failed };

  const fileIds = Array.from(wanted.keys());
  for (let i = 0; i < fileIds.length; i += DOWNLOAD_BATCH_MAX) {
    const batch = fileIds.slice(i, i + DOWNLOAD_BATCH_MAX);
    const refs = new Map<string, DownloadRef>();
    for (const ref of await transport.getDownloads(batch)) {
      if (ref.fileId) refs.set(ref.fileId, ref);
    }
    for (const id of batch) {
      const ref = refs.get(id);
      if (ref === undefined) {
        failed.set(id, "download");
        continue;
      }
      const outcome = await verifyRef(ref, wanted.get(id)!, log, fetchImpl);
      if ("bytes" in outcome) bodies.set(id, outcome.bytes);
      else failed.set(id, outcome.failure);
    }
  }

  log.info("bodies_downloaded", { requested: fileIds.length, downloaded: bodies.size, failed: failed.size });
  return { bodies, failed };
}
