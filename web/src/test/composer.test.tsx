import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { enforceUntilDate, validateDraft, type ComposerDraft } from "../lib/validate";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { formatDate } from "../lib/format";
import { createMockTransport } from "../mock/transport";
import { renderApp } from "./render";

const base: ComposerDraft = {
  artifactId: "",
  ideType: IdeType.CLAUDE_CODE,
  type: ArtifactType.RULE,
  slug: "commit-style",
  name: "Commit style",
  multiFile: false,
  inlineContent: "Use the imperative mood.",
  files: [],
  hookEvent: "",
  hookMatcher: "",
  hookTimeout: "",
  scope: ArtifactScope.USER_GLOBAL,
  repository: "",
  audience: { mode: "org", personIds: [], teamIds: [] },
  enforceUntil: "",
};

function localDay(offsetDays: number): string {
  const d = new Date(Date.now() + offsetDays * 86_400_000);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

describe("composer validation", () => {
  it("accepts a complete draft", () => {
    expect(validateDraft(base)).toEqual({});
  });

  it.each(["", "Upper", "-lead", "has space", "a".repeat(101)])("rejects slug %j", (slug) => {
    expect(validateDraft({ ...base, slug }).slug).toBeDefined();
  });

  it("requires a body on create and on edit", () => {
    expect(validateDraft({ ...base, inlineContent: "" }).inlineContent).toBeDefined();
    expect(validateDraft({ ...base, artifactId: "a_1", inlineContent: "" }).inlineContent).toBeDefined();
  });

  it("bounds a chosen enforcement end by the server's window", () => {
    const limits = { now: Date.now(), maxDays: 90 };
    expect(validateDraft({ ...base, enforceUntil: localDay(30) }, limits).enforceUntil).toBeUndefined();
    expect(validateDraft({ ...base, enforceUntil: localDay(90) }, limits).enforceUntil).toBeUndefined();
    expect(validateDraft({ ...base, enforceUntil: localDay(-1) }, limits).enforceUntil).toMatch(/future/);
    expect(validateDraft({ ...base, enforceUntil: localDay(92) }, limits).enforceUntil).toMatch(/90 days/);
    const last = enforceUntilDate(localDay(90), limits)!;
    expect(last.getTime()).toBeLessThan(limits.now + 90 * 86_400_000);
  });

  it("requires exactly one SKILL.md and safe relative paths for a multi-file skill", () => {
    const skill = { ...base, type: ArtifactType.SKILL, multiFile: true };
    expect(validateDraft({ ...skill, files: [{ relativePath: "run.sh", content: "x" }] }).files).toMatch(/SKILL\.md/);
    const bad = validateDraft({
      ...skill,
      files: [
        { relativePath: "SKILL.md", content: "x" },
        { relativePath: "../escape.sh", content: "x" },
        { relativePath: "-rf", content: "x" },
      ],
    });
    expect(bad["file.1"]).toBeDefined();
    expect(bad["file.2"]).toBeDefined();
    expect(validateDraft({ ...skill, files: [{ relativePath: "SKILL.md", content: "a" }, { relativePath: "SKILL.md", content: "b" }] })["file.1"]).toMatch(/unique/);
  });

  it("rejects several files for a non-skill", () => {
    expect(validateDraft({ ...base, multiFile: true, files: [{ relativePath: "SKILL.md", content: "x" }] }).files).toBeDefined();
  });

  it("checks the hook event against the tool's vocabulary", () => {
    const hook = { ...base, type: ArtifactType.HOOK, inlineContent: "#!/bin/sh" };
    expect(validateDraft(hook).hookEvent).toBeDefined();
    expect(validateDraft({ ...hook, hookEvent: "sessionStart" }).hookEvent).toBeDefined();
    expect(validateDraft({ ...hook, hookEvent: "PreToolUse" }).hookEvent).toBeUndefined();
    expect(validateDraft({ ...hook, hookEvent: "PreToolUse", hookTimeout: "1.5" }).hookTimeout).toBeDefined();
  });

  it("validates the first placement", () => {
    expect(validateDraft({ ...base, scope: ArtifactScope.REPO_LOCAL, repository: "nope" }).repository).toBeDefined();
    expect(validateDraft({ ...base, scope: ArtifactScope.REPO_LOCAL, repository: "example-org/example-repo" }).repository).toBeUndefined();
    expect(validateDraft({ ...base, type: ArtifactType.AGENTS_MD }).scope).toMatch(/repository/);
    expect(validateDraft({ ...base, audience: { mode: "persons", personIds: [], teamIds: [] } }).audience).toBeDefined();
  });
});

describe("composer form", () => {
  it("shows errors and does not save an invalid artifact", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new" });
    await user.type(await screen.findByLabelText("Slug"), "Not Valid");
    await user.click(screen.getByRole("button", { name: "Create artifact" }));
    expect(screen.getByText(/Use lower-case letters/)).toBeInTheDocument();
    expect(screen.getByText("Name is required.")).toBeInTheDocument();
    expect(screen.getByLabelText("Slug")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("heading", { name: "New artifact" })).toBeInTheDocument();
  });

  it("shows the hook note only for hooks, with the server's enabled state", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new" });
    const type = await screen.findByLabelText("Type");
    expect(screen.queryByText(/Hooks run code on every targeted machine/)).not.toBeInTheDocument();
    await user.selectOptions(type, "Hook");
    expect(screen.getByText(/Hooks run code on every targeted machine/)).toBeInTheDocument();
    expect(await screen.findByText(/Hook enforcement is enabled on this server/)).toBeInTheDocument();
    expect(screen.queryByText(/disabled on this server/)).not.toBeInTheDocument();
  });

  it("warns that a hook will be refused when the server disables hooks", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new", transport: (getToken) => createMockTransport(getToken, { latencyMs: 0, hooksEnabled: false }) });
    await user.selectOptions(await screen.findByLabelText("Type"), "Hook");
    expect(await screen.findByText("Hook artifacts are disabled on this server.")).toBeInTheDocument();
    expect(screen.queryByText(/Hook enforcement is enabled/)).not.toBeInTheDocument();
  });

  it("creates an artifact with its first placement and lands on its page", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new" });
    await user.selectOptions(await screen.findByLabelText("Type"), "Rule");
    await user.type(screen.getByLabelText("Slug"), "test-style");
    await user.type(screen.getByLabelText("Name"), "Test style");
    await user.type(screen.getByLabelText("Body"), "Be brief.");
    await user.click(screen.getByRole("button", { name: "Create artifact" }));
    expect(await screen.findByRole("heading", { name: "Test style" })).toBeInTheDocument();
    const targets = screen.getByRole("region", { name: "Targets" });
    expect(within(targets).getByText("Everyone")).toBeInTheDocument();
  });

  it("pre-fills an edit with the stored body and saves it back", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/a_commit/edit" });
    const body = await screen.findByLabelText("Body");
    expect(body).toHaveValue("---\ndescription: Commit message conventions\nalwaysApply: true\n---\n\nWrite commit subjects in the imperative mood, at most 72 characters.\n");
    expect(screen.queryByText(/stored body is kept/)).not.toBeInTheDocument();
    await user.clear(body);
    await user.type(body, "Use short subjects.");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("heading", { name: "Commit message style" })).toBeInTheDocument();
    await user.click(screen.getByRole("link", { name: "Edit" }));
    expect(await screen.findByLabelText("Body")).toHaveValue("Use short subjects.");
  });

  it("pre-fills every file of a multi-file skill", async () => {
    renderApp({ path: "/artifacts/a_review/edit" });
    expect(await screen.findByLabelText("File 1 path")).toHaveValue("SKILL.md");
    expect(screen.getByLabelText("File 2 path")).toHaveValue("scripts/diffstat.sh");
    expect(screen.getByLabelText("File 2 content")).toHaveValue('#!/bin/sh\ngit diff --stat "$@"\n');
  });

  it("refuses an empty body on edit", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/a_commit/edit" });
    await user.clear(await screen.findByLabelText("Body"));
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("Content is required.")).toBeInTheDocument();
  });

  it("sends a chosen enforcement end and rejects one past the maximum", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new" });
    await user.selectOptions(await screen.findByLabelText("Type"), "Rule");
    await user.type(screen.getByLabelText("Slug"), "long-rule");
    await user.type(screen.getByLabelText("Name"), "Long rule");
    await user.type(screen.getByLabelText("Body"), "Be brief.");
    expect(await screen.findByText(/Empty enforces for 15 days from this save; at most 90 days ahead/)).toBeInTheDocument();
    const until = screen.getByLabelText("Enforce until");
    await user.type(until, localDay(120));
    await user.click(screen.getByRole("button", { name: "Create artifact" }));
    expect(screen.getByText("Pick a date at most 90 days ahead.")).toBeInTheDocument();

    await user.clear(until);
    await user.type(until, localDay(40));
    await user.click(screen.getByRole("button", { name: "Create artifact" }));
    expect(await screen.findByRole("heading", { name: "Long rule" })).toBeInTheDocument();
    const day = new Date(`${localDay(40)}T12:00:00`);
    expect(screen.getAllByText(new RegExp(`Enforced until ${formatDate(timestampFromDate(day))}`)).length).toBeGreaterThan(0);
  });

  it("previews the install path for the chosen scope", async () => {
    const user = userEvent.setup();
    renderApp({ path: "/artifacts/new" });
    await user.selectOptions(await screen.findByLabelText("Type"), "Rule");
    await user.type(screen.getByLabelText("Slug"), "style");
    expect(await screen.findByText("~/.claude/rules/style.md")).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /Repository/ }));
    expect(await screen.findByText("<checkout>/.claude/rules/style.md")).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Tool"), "Codex");
    await user.selectOptions(screen.getByLabelText("Type"), "Hook");
    await user.click(screen.getByRole("radio", { name: /User \(machine-wide\)/ }));
    expect(await screen.findByText("~/.codex/hooks/style.sh")).toBeInTheDocument();
  });
});
