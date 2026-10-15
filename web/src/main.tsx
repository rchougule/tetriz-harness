import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";
import type { Transport } from "@connectrpc/connect";
import { AppProviders } from "./api/context";
import { mockRequested } from "./api/mockFlag";
import { createServerTransport } from "./api/transport";
import { AppRoutes } from "./App";
import { ToastProvider } from "./components/Toast";
import "./styles.css";

async function bootstrap() {
  let makeTransport: (getToken: () => string | null) => Transport = createServerTransport;
  let mock = false;
  // Kept inline so production builds drop the branch, and with it the mock chunk.
  if (import.meta.env.DEV || import.meta.env.VITE_ENABLE_MOCK === "1") {
    if (mockRequested()) {
      const { createMockTransport } = await import("./mock/transport");
      makeTransport = (getToken) => createMockTransport(getToken);
      mock = true;
    }
  }
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <AppProviders makeTransport={makeTransport} mock={mock}>
        <ToastProvider>
          <BrowserRouter>
            <AppRoutes />
          </BrowserRouter>
        </ToastProvider>
      </AppProviders>
    </StrictMode>,
  );
}

void bootstrap();
