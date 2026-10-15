import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRouterTransport, type ServiceImpl } from "@connectrpc/connect";
import { AdminService } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { authInterceptor } from "../api/transport";
import { createMockState } from "../mock/transport";
import { renderApp } from "./render";

// countingTransport serves the mock and records which RPCs the page made.
function countingTransport(calls: string[]) {
  return (getToken: () => string | null) => {
    const impl = createMockState();
    const wrapped = Object.fromEntries(
      Object.entries(impl).map(([name, fn]) => [name, (...args: unknown[]) => (calls.push(name), (fn as (...a: unknown[]) => unknown)(...args))]),
    ) as ServiceImpl<typeof AdminService>;
    return createRouterTransport((r) => r.service(AdminService, wrapped), { transport: { interceptors: [authInterceptor(getToken)] } });
  };
}

describe("artifact list", () => {
  it("shows server-side status counts without a status call per artifact", async () => {
    const calls: string[] = [];
    renderApp({ path: "/artifacts", transport: countingTransport(calls) });
    const row = (await screen.findByRole("link", { name: "Code review checklist" })).closest("tr")!;
    const counts = within(row).getByRole("list", { name: "Status counts" });
    expect(counts).toHaveTextContent("1 failed");
    expect(counts).toHaveTextContent("1 excluded");
    expect(counts).toHaveTextContent("2 pending");
    expect(counts).toHaveTextContent("2 installed");
    expect(calls).not.toContain("getEnforcementStatus");
  });
});

describe("retracted artifacts", () => {
  it("are listed only with Show retracted, without a retract action", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts" });
    await screen.findByRole("link", { name: "Code review checklist" });
    expect(screen.queryByRole("link", { name: "Legacy lint rules" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: "Show retracted" }));
    const row = (await screen.findByRole("link", { name: "Legacy lint rules" })).closest("tr")!;
    expect(within(row).getByText(/^Retracted/)).toBeInTheDocument();
    expect(within(row).getByRole("list", { name: "Status counts" })).toHaveTextContent("1 removal pending");
    expect(within(row).queryByRole("button", { name: /Retract/ })).not.toBeInTheDocument();
  });

  it("open on a detail page that tracks removal", async () => {
    renderApp({ path: "/artifacts/a_legacy" });
    expect(await screen.findByRole("heading", { name: "Legacy lint rules" })).toBeInTheDocument();
    expect(screen.getByText(/^Retracted on/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add placement" })).not.toBeInTheDocument();
    const status = screen.getByRole("region", { name: "Per-person status" });
    const grace = (await within(status).findByText("Grace Hopper")).closest("tr")!;
    expect(within(grace).getByText("Removal pending")).toBeInTheDocument();
    const barbara = within(status).getByText("Barbara Liskov").closest("tr")!;
    expect(within(barbara).getByText("Removed")).toBeInTheDocument();
  });

  it("stay on their page after a retraction", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/a_guide" });
    await user.click(await screen.findByRole("button", { name: "Retract" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Retract" }));
    expect(await screen.findByText(/^Retracted on/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Repository guide" })).toBeInTheDocument();
  });
});
