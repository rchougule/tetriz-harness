// Shared fixtures for the presigned-URL redaction guards.

/** A real SignatureDoesNotMatch body: its CanonicalRequest reproduces the presigned query string. */
export const SIGNATURE_ERROR = [
  '<?xml version="1.0" encoding="UTF-8"?>',
  "<Error><Code>SignatureDoesNotMatch</Code>",
  "<Message>The request signature we calculated does not match the signature you provided.</Message>",
  "<AWSAccessKeyId>ASIALEAKKEY</AWSAccessKeyId>",
  "<StringToSign>AWS4-HMAC-SHA256\n20260803T073811Z\n20260803/us-east-1/s3/aws4_request\nabc</StringToSign>",
  "<CanonicalRequest>PUT\n/bucket/key\n",
  "X-Amz-Credential=ASIALEAKKEY%2F20260803%2Fus-east-1%2Fs3%2Faws4_request",
  "&X-Amz-Security-Token=FwoGZXIvYXdzEXAMPLETOKEN",
  "&X-Amz-Signature=deadbeef</CanonicalRequest></Error>",
].join("");

/** `aws4_request` is public, but appears only inside StringToSign/CanonicalRequest: a cheap leak canary. */
export const FORBIDDEN_TOKENS = [
  "X-Amz-Credential",
  "X-Amz-Security-Token",
  "X-Amz-Signature",
  "ASIALEAKKEY",
  "FwoGZXIvYXdzEXAMPLETOKEN",
  "CanonicalRequest",
  "StringToSign",
  "aws4_request",
];

export function assertNoCredentialMaterial(haystack: string, ok: (cond: boolean, msg: string) => void): void {
  // An empty haystack would pass every check below.
  ok(haystack.length > 0, "haystack must be non-empty — nothing was asserted");
  for (const token of FORBIDDEN_TOKENS) {
    ok(!haystack.includes(token), `must not contain ${token}`);
  }
}
