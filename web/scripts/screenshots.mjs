// Captures every screen against the mock in light and dark: node scripts/screenshots.mjs <baseUrl> <outDir>
import { mkdir } from "node:fs/promises";
import { chromium } from "playwright";

const base = process.argv[2] ?? "http://localhost:5173";
const out = process.argv[3] ?? "screenshots";
const only = process.argv[4];
await mkdir(out, { recursive: true });

const shots = [
  { name: "signin", path: "/", signIn: false },
  { name: "signin-rejected", path: "/", signIn: false, run: async (p) => { await p.fill("input[type=password]", "invalid"); await p.click("button[type=submit]"); await p.getByText("That token was rejected.").waitFor(); } },
  { name: "artifacts", path: "/artifacts" },
  { name: "artifact-retract-dialog", path: "/artifacts", run: async (p) => { await p.getByRole("button", { name: "Retract Code review checklist" }).click(); await p.getByRole("dialog").waitFor(); } },
  { name: "composer-new", path: "/artifacts/new", run: async (p) => { await p.getByLabel("Slug").fill("onboarding"); await p.getByLabel("Name").fill("Onboarding guide"); await p.waitForTimeout(600); } },
  { name: "composer-errors", path: "/artifacts/new", run: async (p) => { await p.getByLabel("Slug").fill("Bad Slug"); await p.getByRole("button", { name: "Create artifact" }).first().click(); await p.waitForTimeout(200); } },
  { name: "composer-multifile-skill", path: "/artifacts/new", run: async (p) => { await p.getByLabel("Slug").fill("code-review"); await p.getByLabel("Name").fill("Code review"); await p.getByLabel("Several files").check(); await p.getByRole("button", { name: "Add file" }).click(); await p.getByLabel("File 2 path").fill("scripts/diffstat.sh"); await p.waitForTimeout(600); } },
  { name: "composer-hook", path: "/artifacts/new", run: async (p) => { await p.getByLabel("Type").selectOption({ label: "Hook" }); await p.getByLabel("Slug").fill("block-secrets"); await p.waitForTimeout(600); } },
  { name: "composer-edit", path: "/artifacts/a_secrets/edit" },
  { name: "artifact-detail", path: "/artifacts/a_review" },
  { name: "artifact-detail-repo", path: "/artifacts/a_commit" },
  { name: "artifact-detail-expired", path: "/artifacts/a_notes" },
  { name: "artifact-targets-dialog", path: "/artifacts/a_commit", run: async (p) => { await p.getByRole("button", { name: /^Edit targets/ }).first().click(); await p.getByRole("dialog").waitFor(); await p.waitForTimeout(300); } },
  { name: "artifacts-retracted", path: "/artifacts", run: async (p) => { await p.getByRole("checkbox", { name: "Show retracted" }).check(); await p.getByRole("link", { name: "Legacy lint rules" }).waitFor(); } },
  { name: "artifact-detail-retracted", path: "/artifacts/a_legacy" },
  { name: "composer-edit-skill", path: "/artifacts/a_review/edit" },
  { name: "composer-enforce-until-error", path: "/artifacts/new", run: async (p) => { await p.getByLabel("Slug").fill("long-rule"); await p.getByLabel("Name").fill("Long rule"); await p.getByRole("textbox", { name: "SKILL.md" }).fill("x"); await p.getByLabel("Enforce until").fill("2099-01-01"); await p.getByRole("button", { name: "Create artifact" }).first().click(); await p.waitForTimeout(300); } },
  { name: "settings", path: "/settings" },
  { name: "paused-artifacts", path: "/settings", run: async (p) => { await p.getByRole("button", { name: "Pause enforcement" }).click(); await p.getByRole("dialog").getByRole("button", { name: "Pause enforcement" }).click(); await p.getByText("Enforcement is paused.").first().waitFor(); await p.getByRole("link", { name: "Artifacts" }).click(); await p.waitForTimeout(400); } },
  { name: "narrow-artifacts", path: "/artifacts", viewport: { width: 760, height: 900 } },
  { name: "w1024-artifacts", path: "/artifacts", viewport: { width: 1024, height: 800 } },
  { name: "w1024-artifact-detail", path: "/artifacts/a_review", viewport: { width: 1024, height: 800 } },
];

const browser = await chromium.launch();
const errors = [];
for (const scheme of ["light", "dark"]) {
  for (const s of shots) {
    if (only && !s.name.includes(only)) continue;
    const ctx = await browser.newContext({ colorScheme: scheme, viewport: s.viewport ?? { width: 1280, height: 860 }, deviceScaleFactor: 1 });
    const page = await ctx.newPage();
    page.on("pageerror", (e) => errors.push(`${s.name}: ${e.message}`));
    page.on("console", (m) => { if (m.type() === "error") errors.push(`${s.name}: ${m.text()}`); });
    await page.goto(`${base}/?mock=1`);
    if (s.signIn !== false) {
      await page.fill("input[type=password]", "example-admin-token");
      await page.click("button[type=submit]");
      await page.getByRole("navigation", { name: "Main" }).waitFor();
      await page.evaluate((path) => { window.history.pushState({}, "", path); window.dispatchEvent(new PopStateEvent("popstate")); }, s.path);
      await page.waitForTimeout(500);
    }
    if (s.run) await s.run(page);
    await page.screenshot({ path: `${out}/${s.name}-${scheme}.png`, fullPage: !s.name.includes("dialog") });
    await ctx.close();
  }
}
await browser.close();
if (errors.length) { console.log("ERRORS:\n" + errors.join("\n")); process.exitCode = 1; }
else console.log("ok");
