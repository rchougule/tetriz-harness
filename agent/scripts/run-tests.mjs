// Node 20's `--test` takes no globs, so collect the compiled test files explicitly.
import { readdirSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import process from "node:process";

function collect(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name);
    if (e.isDirectory()) return collect(p);
    return e.name.endsWith(".test.js") ? [p] : [];
  });
}

const files = collect(".test-build/test").sort();
const res = spawnSync(process.execPath, ["--test", ...files], { stdio: "inherit" });
process.exit(res.status ?? 1);
