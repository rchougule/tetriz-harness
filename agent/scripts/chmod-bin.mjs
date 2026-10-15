import { chmodSync } from "node:fs";

chmodSync("dist/cli/main.js", 0o755);
