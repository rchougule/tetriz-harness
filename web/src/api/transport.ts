import type { Interceptor, Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

export function authInterceptor(getToken: () => string | null): Interceptor {
  return (next) => async (req) => {
    const token = getToken();
    if (token) req.header.set("Authorization", `Bearer ${token}`);
    return next(req);
  };
}

export function createServerTransport(getToken: () => string | null): Transport {
  return createConnectTransport({
    baseUrl: window.location.origin,
    useBinaryFormat: false,
    interceptors: [authInterceptor(getToken)],
  });
}
