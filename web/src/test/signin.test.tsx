import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { AdminService } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { authInterceptor } from "../api/transport";
import { clearToken, readToken } from "../api/token";
import { renderApp } from "./render";

describe("sign-in", () => {
  beforeEach(() => clearToken());

  it("rejects a bad token and admits a good one", async () => {
    const user = userEvent.setup();
    renderApp({ token: null });
    await user.type(screen.getByLabelText("Admin token"), "invalid");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText("That token was rejected.")).toBeInTheDocument();
    expect(readToken()).toBeNull();

    await user.clear(screen.getByLabelText("Admin token"));
    await user.type(screen.getByLabelText("Admin token"), "good-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Artifacts" })).toBeInTheDocument();
    expect(window.sessionStorage.getItem("hcp.adminToken")).toBe("good-token");
  });

  it("returns to sign-in when a later call is unauthenticated", async () => {
    const seen: string[] = [];
    let releaseRoster = () => {};
    const rosterGate = new Promise<void>((r) => (releaseRoster = r));
    const transport = (getToken: () => string | null) =>
      createRouterTransport(
        (r) =>
          r.service(AdminService, {
            listArtifacts(_req, ctx) {
              seen.push(ctx.requestHeader.get("Authorization") ?? "");
              return { artifacts: [], enforcementPaused: false };
            },
            getSettings: () => ({ enforcementPaused: false, hooksEnabled: false }),
            // The roster read is held, then rejected, as if the token were revoked mid-session.
            async listPersons() {
              await rosterGate;
              throw new ConnectError("token revoked", Code.Unauthenticated);
            },
            listTeams: () => ({ teams: [] }),
          }),
        { transport: { interceptors: [authInterceptor(getToken)] } },
      );
    renderApp({ token: "admin-token", transport });
    expect(await screen.findByRole("heading", { name: "Artifacts" })).toBeInTheDocument();
    expect(seen[0]).toBe("Bearer admin-token");

    releaseRoster();
    await waitFor(() => expect(screen.getByLabelText("Admin token")).toBeInTheDocument());
    expect(screen.getByRole("alert")).toHaveTextContent("rejected the admin token");
    expect(readToken()).toBeNull();
  });

  it("proves the token with GetSettings", async () => {
    const user = userEvent.setup();
    const calls: string[] = [];
    const transport = (getToken: () => string | null) =>
      createRouterTransport(
        (r) =>
          r.service(AdminService, {
            getSettings(_req, ctx) {
              calls.push(`getSettings ${ctx.requestHeader.get("Authorization")}`);
              return { enforcementPaused: false, hooksEnabled: false };
            },
            listArtifacts: () => ({ artifacts: [] }),
            listPersons: () => ({ persons: [] }),
            listTeams: () => ({ teams: [] }),
          }),
        { transport: { interceptors: [authInterceptor(getToken)] } },
      );
    renderApp({ token: null, transport });
    await user.type(screen.getByLabelText("Admin token"), "checked-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Artifacts" })).toBeInTheDocument();
    expect(calls[0]).toBe("getSettings Bearer checked-token");
  });

  it("signs out on request", async () => {
    const user = userEvent.setup();
    renderApp();
    await user.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(screen.getByLabelText("Admin token")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
