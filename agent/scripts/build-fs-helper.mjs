import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, URL } from "node:url";
import process from "node:process";

const root = fileURLToPath(new URL("../../", import.meta.url));
const agentDir = fileURLToPath(new URL("../", import.meta.url));
if (!["darwin", "linux"].includes(process.platform)) {
  throw new Error("The agent filesystem helper supports macOS and Linux; build on the target platform.");
}
const run = (args) => {
  const result = spawnSync("go", args, { cwd: root, encoding: "utf8", env: { ...process.env, CGO_ENABLED: "0", GOOS: process.platform, GOARCH: process.arch === "x64" ? "amd64" : process.arch } });
  if (result.error || result.status !== 0) throw new Error(`building the agent requires Go (see go.mod): ${result.error?.message ?? result.stderr}`);
  return result.stdout.trim();
};
const required = readFileSync(join(root, "go.mod"), "utf8").match(/^toolchain go([\d.]+)$/m)?.[1];
const actual = run(["env", "GOVERSION"]).replace(/^go/, "");
const numeric = (v) => v.split(".").map(Number);
if (!required || !/^\d+\.\d+\.\d+$/.test(actual) || numeric(actual).some((n, i, a) => a.slice(0, i).every((x, j) => x === numeric(required)[j]) && n < numeric(required)[i])) {
  throw new Error(`The filesystem helper requires the patched toolchain go${required}; selected go${actual}. Enable Go toolchain selection.`);
}
const output = join(agentDir, "dist/native/fs-helper");
mkdirSync(dirname(output), { recursive: true });
run(["build", "-trimpath", "-ldflags=-s -w", "-o", output, "./agent/native/fshelper"]);
const licenses = join(agentDir, "dist/native/licenses");
mkdirSync(licenses, { recursive: true });
const goroot = run(["env", "GOROOT"]);
for (const name of ["LICENSE", "PATENTS"]) {
  const destination = join(licenses, name);
  rmSync(destination, { force: true });
  writeFileSync(destination, readFileSync(join(goroot, name)));
}
writeFileSync(join(licenses, "PROJECT-LICENSE"), readFileSync(join(root, "LICENSE")));
writeFileSync(join(licenses, "NOTICE"), `fs-helper source is MIT-licensed (PROJECT-LICENSE). It includes the ${actual} Go standard library; its BSD license and patent grant accompany this executable.\n`);
