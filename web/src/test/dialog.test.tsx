import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Dialog } from "../components/Dialog";
import { mockWith, renderApp } from "./render";

describe("dialog focus", () => {
  it("cycles only the enabled controls of an existing placement, also while saving, and returns focus", async () => {
    const user = userEvent.setup();
    let release = () => {};
    const held = new Promise<void>((r) => (release = r));
    renderApp({
      path: "/artifacts/a_secrets",
      transport: mockWith((impl) => ({
        setTargets: async (req, ctx) => {
          await held;
          return impl.setTargets(req, ctx);
        },
      })),
    });
    const trigger = await screen.findByRole("button", { name: "Edit targets for User (machine-wide)" });
    await user.click(trigger);
    const d = within(screen.getByRole("dialog", { name: "Edit targets" }));

    // The scope fieldset is disabled for an existing placement, so focus starts on the audience.
    expect(d.getByRole("radio", { name: /^User \(machine-wide\)/ })).toBeDisabled();
    const teams = d.getByRole("radio", { name: /^Teams/ });
    expect(teams).toHaveFocus();
    expect(trigger.closest("[inert]")).not.toBeNull();

    const [platform, mobile, docs] = ["Platform", "Mobile", "Docs"].map((n) => d.getByRole("checkbox", { name: new RegExp(`^${n}`) }));
    const cancel = d.getByRole("button", { name: "Cancel" });
    const save = d.getByRole("button", { name: "Save targets" });
    for (const el of [platform, mobile, docs, cancel, save, teams]) {
      await user.tab();
      expect(el).toHaveFocus();
    }
    for (const el of [save, cancel, docs, mobile, platform, teams]) {
      await user.tab({ shift: true });
      expect(el).toHaveFocus();
    }

    // While the save is pending its buttons are disabled and drop out of the cycle.
    await user.click(save);
    expect(d.getByRole("button", { name: "Saving…" })).toBeDisabled();
    expect(cancel).toBeDisabled();
    await user.tab();
    expect(teams).toHaveFocus();
    await user.tab({ shift: true });
    expect(docs).toHaveFocus();
    await user.tab();
    expect(teams).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    release();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(trigger).toHaveFocus();
    expect(trigger.closest("[inert]")).toBeNull();
  });

  it("focuses the marked control and returns focus on Escape", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/a_guide" });
    const trigger = await screen.findByRole("button", { name: "Retract" });
    await user.click(trigger);
    expect(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it("holds focus on itself when no control is enabled", async () => {
    const user = userEvent.setup();
    render(
      <>
        <button type="button">Behind</button>
        <Dialog open busy title="Working" onClose={() => {}} footer={<button type="button" disabled>Done</button>} />
      </>,
    );
    const dialog = screen.getByRole("dialog", { name: "Working" });
    expect(dialog).toHaveFocus();
    await user.tab();
    expect(dialog).toHaveFocus();
    await user.tab({ shift: true });
    expect(dialog).toHaveFocus();
  });
});
