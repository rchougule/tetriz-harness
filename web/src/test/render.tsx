import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { createRouterTransport, type ServiceImpl, type Transport } from "@connectrpc/connect";
import { AdminService } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { AppProviders } from "../api/context";
import { writeToken } from "../api/token";
import { authInterceptor } from "../api/transport";
import { AppRoutes } from "../App";
import { ToastProvider } from "../components/Toast";
import { createMockState, createMockTransport } from "../mock/transport";

interface Options {
  path?: string;
  token?: string | null;
  transport?: (getToken: () => string | null) => Transport;
}

export function renderApp({ path = "/artifacts", token = "test-token", transport }: Options = {}) {
  if (token) writeToken(token);
  const make = transport ?? ((getToken: () => string | null) => createMockTransport(getToken, { latencyMs: 0 }));
  return render(
    <AppProviders makeTransport={make} mock>
      <ToastProvider>
        <MemoryRouter initialEntries={[path]}>
          <AppRoutes />
        </MemoryRouter>
      </ToastProvider>
    </AppProviders>,
  );
}

type Impl = ServiceImpl<typeof AdminService>;

// mockWith serves the mock with some RPCs replaced, to fail them or hold them open.
export function mockWith(override: (impl: Impl) => Partial<Impl>) {
  return (getToken: () => string | null) => {
    const impl = createMockState();
    return createRouterTransport((r) => r.service(AdminService, { ...impl, ...override(impl) }), {
      transport: { interceptors: [authInterceptor(getToken)] },
    });
  };
}
