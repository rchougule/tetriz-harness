import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "./render";

describe("pause enforcement", () => {
  it("shows a banner on every page while paused and clears it on resume", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/settings" });
    expect(await screen.findByText("Running")).toBeInTheDocument();
    expect(screen.queryByText("Enforcement is paused.")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Pause enforcement" }));
    const dialog = await screen.findByRole("dialog", { name: "Pause enforcement?" });
    await user.click(within(dialog).getByRole("button", { name: "Pause enforcement" }));
    expect(await screen.findByText("Enforcement is paused.")).toBeInTheDocument();

    for (const page of ["Artifacts", "Settings"]) {
      await user.click(screen.getByRole("link", { name: page }));
      expect(screen.getByText("Enforcement is paused.")).toBeInTheDocument();
    }

    await user.click(screen.getByRole("link", { name: "Settings" }));
    await user.click(await screen.findByRole("button", { name: "Resume enforcement" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Resume enforcement" }));
    expect(await screen.findByText("Running")).toBeInTheDocument();
    expect(screen.queryByText("Enforcement is paused.")).not.toBeInTheDocument();
  });

  it("shows the server's hook opt-in, window and version", async () => {
    renderApp({ path: "/settings" });
    const server = await screen.findByRole("region", { name: "Server" });
    expect(within(server).getByText("Enabled")).toBeInTheDocument();
    expect(within(server).getByText(/15 days from each save/)).toBeInTheDocument();
    expect(within(server).getByText(/up to 90 days ahead/)).toBeInTheDocument();
    expect(within(server).getByText("mock")).toBeInTheDocument();
  });

  it("cancels without pausing", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/settings" });
    await user.click(await screen.findByRole("button", { name: "Pause enforcement" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText("Running")).toBeInTheDocument();
  });
});
