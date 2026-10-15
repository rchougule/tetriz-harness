// Presigned URLs, and the error bodies object stores return for them, carry live credentials.

/** Keep origin + path; drop the query (and userinfo) that can carry a credential. */
export function maskPresignedUrl(url: string): string {
  try {
    const parsed = new URL(url);
    const base = `${parsed.origin}${parsed.pathname}`;
    // Only claim a redaction that happened, so a query-less URL is not mistaken for a signed one.
    return parsed.search ? `${base}?[SIGNED_PARAMS_REDACTED]` : base;
  } catch {
    return "[INVALID_URL]";
  }
}

/** Mask `url` inside a message: fetch() rejects a bad URL with the raw URL in its error text. */
export function redactUrlIn(message: string, url: string): string {
  return url ? message.split(url).join(maskPresignedUrl(url)) : message;
}

/**
 * Allow-list only <Code> and <Message> from an S3-style error body; the rest echoes the signed query
 * string, so a block-list would leak whatever field is added next.
 */
export function s3ErrorSummary(body: string): string {
  // Capture unbounded then truncate: a bounded quantifier would drop a long value entirely.
  const code = (/<Code>([^<]*)<\/Code>/.exec(body)?.[1] ?? "").slice(0, 100);
  const message = (/<Message>([^<]*)<\/Message>/.exec(body)?.[1] ?? "").slice(0, 300);
  // A real value names signing params but never assigns them; an `=` means echoed query material.
  const dirty = (v: string): boolean => /x-amz-[a-z0-9-]+=/i.test(v);
  return [dirty(code) ? "" : code, dirty(message) ? "" : message].filter(Boolean).join(": ");
}

/** A local directory and the stable name a reported reason carries in its place. */
export interface PathRoot {
  prefix: string;
  replacement: string;
}

const escapeRegExp = (s: string): string => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

function basenameOf(path: string): string {
  const trimmed = path.replace(/\/+$/, "");
  return trimmed === "" ? path : trimmed.slice(trimmed.lastIndexOf("/") + 1);
}

/**
 * Rewrite absolute local paths in an error text before it leaves the machine: a known root becomes
 * its replacement (the longest root wins, so a checkout under home reads as its repository), and any
 * other absolute path keeps only its basename. Error texts quote paths that may contain spaces, so a
 * quoted path is taken whole.
 */
export function scrubLocalPaths(message: string, roots: PathRoot[]): string {
  let out = message;
  const ordered = roots
    .filter((r) => r.prefix.startsWith("/") && r.prefix.replace(/\/+$/, "") !== "")
    .map((r) => ({ prefix: r.prefix.replace(/\/+$/, ""), replacement: r.replacement }))
    .sort((a, b) => b.prefix.length - a.prefix.length);
  for (const { prefix, replacement } of ordered) {
    // Only at the start of a path and on a segment boundary: /home/dev must not match /home/developer.
    const re = new RegExp(`(?<![\\w.~/-])${escapeRegExp(prefix)}(?=$|/|[^\\w.-])`, "g");
    out = out.replace(re, () => replacement);
  }
  out = out.replace(/(['"`])(\/[^'"`]*)\1/g, (_m, q: string, path: string) => `${q}${basenameOf(path)}${q}`);
  return out.replace(/(^|[\s(=,[])(\/[^\s'"`),\]]+)/g, (_m, lead: string, path: string) => `${lead}${basenameOf(path)}`);
}
