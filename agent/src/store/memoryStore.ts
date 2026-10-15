import type { AgentState } from "../engine/state.js";
import type { Store } from "../ports.js";

/** In-process store; copies on the way in and out so callers cannot alias its contents. */
export class MemoryStore implements Store {
  private state: AgentState | null;

  constructor(initial: AgentState | null = null) {
    this.state = initial === null ? null : structuredClone(initial);
  }

  async load(): Promise<AgentState | null> {
    return this.state === null ? null : structuredClone(this.state);
  }

  async save(state: AgentState): Promise<void> {
    this.state = structuredClone(state);
  }
}
