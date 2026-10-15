# @harness-control-plane/agent

The agent for [Harness Control Plane](https://github.com/tetriz-ai/tetriz-harness). It runs on each developer machine, writes the skills, hooks, rules, agents, `CLAUDE.md` and `AGENTS.md` files the server enforces for that person, and reports back what it found. It is a `harness-agent` CLI and a Node library, from the same code.

Build on macOS or Linux with Node 22 or later, pnpm 10 and Go toolchain selection enabled for the patched version in `go.mod`. Build it from the root of a clone:

```sh
pnpm install --frozen-lockfile && pnpm -r build
```

The build bundles a native filesystem helper and its notices under `agent/dist/native/`. Keep these files when packaging the CLI or library. Running the built agent needs Node and the helper, not Go.

Run one pass from the CLI:

```sh
node agent/dist/cli/main.js sync --server http://127.0.0.1:8080 --token "$TOKEN"
```

To run it as `harness-agent` from any terminal, see [Installing the command](../docs/reference.md#installing-the-command).

To embed it in a Node app that already runs on each machine, link it from the clone with `pnpm add link:../tetriz-harness/agent`, then:

```ts
import { ConnectTransport, JsonFileStore, createAgent } from "@harness-control-plane/agent";

const agent = createAgent({
  transport: new ConnectTransport({ server: "http://127.0.0.1:8080", token: process.env.HARNESS_AGENT_TOKEN ?? "" }),
  store: new JsonFileStore(),
});
const result = await agent.sync();
console.log(`fetched=${result.fetched} failures=${result.applyFailures.length}`);
```

The [control plane guide](../docs/control-plane.md) has the quick start, the desktop-app setup and the known limits. The [reference](../docs/reference.md) gives each CLI flag, exit code and install path.
