import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "./render";

describe("navigation", () => {
  it("offers no people or device management, which the host owns", async () => {
    renderApp();
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getAllByRole("link").map((l) => l.textContent)).toEqual(["Artifacts", "Settings"]);
  });

  it("starts the tab order with a link that skips to the main content", async () => {
    const user = userEvent.setup();
    renderApp();
    await screen.findByRole("heading", { name: "Artifacts" });
    await user.tab();
    expect(screen.getByRole("link", { name: "Skip to main content" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("main")).toHaveFocus();
    await user.tab();
    expect(within(screen.getByRole("main")).getByRole("checkbox", { name: "Show retracted" })).toHaveFocus();
  });

  it.each(["/people", "/devices"])("sends %s to the artifacts list", async (path) => {
    renderApp({ path });
    expect(await screen.findByRole("heading", { name: "Artifacts" })).toBeInTheDocument();
  });
});
