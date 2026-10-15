import { Code, ConnectError } from "@connectrpc/connect";

export function isUnauthenticated(err: unknown): boolean {
  return ConnectError.from(err).code === Code.Unauthenticated;
}

export function errorMessage(err: unknown): string {
  const ce = ConnectError.from(err);
  return ce.rawMessage || ce.message || "Request failed";
}

export function isNotFound(err: unknown): boolean {
  return ConnectError.from(err).code === Code.NotFound;
}
