import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Code, ConnectError } from "@connectrpc/connect";
import { mockWith, renderApp } from "./render";

// Queries retry twice with a backoff; fake timers skip it.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"], shouldAdvanceTime: true });
});
afterEach(() => {
  vi.useRealTimers();
});

const unavailable = () => new ConnectError("directory unavailable", Code.Unavailable);

describe("audience picker", () => {
  it("shows a failed team list with Retry and keeps the chosen teams", async () => {
    let down = true;
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    renderApp({
      path: "/artifacts/a_commit",
      transport: mockWith((impl) => ({
        listTeams: (req, ctx) => {
          if (down) throw unavailable();
          return impl.listTeams(req, ctx);
        },
      })),
    });
    await user.click(await screen.findByRole("button", { name: "Edit targets for example-org/example-repo" }));
    const d = within(screen.getByRole("dialog", { name: "Edit targets" }));
    expect(d.getByRole("status")).toHaveTextContent("Loading teams…");

    await act(() => vi.advanceTimersByTimeAsync(5_000));
    const alert = d.getByRole("alert");
    expect(alert).toHaveTextContent("Could not load teams: directory unavailable");
    expect(d.queryByRole("checkbox")).not.toBeInTheDocument();

    down = false;
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await d.findByRole("checkbox", { name: /^Platform/ })).toBeChecked();
    expect(d.getByRole("checkbox", { name: /^Mobile/ })).not.toBeChecked();
    expect(d.getByRole("radio", { name: /^Teams/ })).toBeChecked();
    expect(d.queryByRole("alert")).not.toBeInTheDocument();

    await user.click(d.getByRole("button", { name: "Save targets" }));
    expect(await screen.findByText(/^Targets saved/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "Targets" })).getByText("Platform team")).toBeInTheDocument();
  });

  it("shows a failed people list with Retry and keeps the chosen people", async () => {
    let down = true;
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    renderApp({
      path: "/artifacts/a_guide",
      transport: mockWith((impl) => ({
        listEligibleEngineers: (req, ctx) => {
          if (down) throw unavailable();
          return impl.listEligibleEngineers(req, ctx);
        },
      })),
    });
    await user.click(await screen.findByRole("button", { name: "Edit targets for example-org/example-repo" }));
    const d = within(screen.getByRole("dialog", { name: "Edit targets" }));
    expect(d.getByRole("status")).toHaveTextContent("Loading people…");

    await act(() => vi.advanceTimersByTimeAsync(5_000));
    const alert = d.getByRole("alert");
    expect(alert).toHaveTextContent("Could not load people: directory unavailable");

    down = false;
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await d.findByRole("checkbox", { name: /^Ada Lovelace/ })).toBeChecked();
    expect(d.getByRole("checkbox", { name: /^Grace Hopper/ })).toBeChecked();
    expect(d.getByRole("checkbox", { name: /^Alan Turing/ })).not.toBeChecked();
    expect(d.queryByRole("alert")).not.toBeInTheDocument();
  });
});
