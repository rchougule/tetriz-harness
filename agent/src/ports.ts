import type { DownloadRef, EnforcedSet, StateReport } from "./contract.js";
import type { AgentState } from "./engine/state.js";

/**
 * The control-plane connection. A throw from `getEnforcedSet` means "no fresh desired state": the
 * pass makes no writes. A throw from `getDownloads` aborts the pass (retried next pass); a file id
 * absent from its result is a per-file download failure.
 */
export interface Transport {
  getEnforcedSet(): Promise<EnforcedSet>;
  getDownloads(fileIds: string[]): Promise<DownloadRef[]>;
  report(state: StateReport): Promise<void>;
}

/**
 * Durable home for the agent's snapshot, ledger and pass history. `load` returns null when empty. A
 * store shared between processes implements `lock`, which a pass holds from its load to its save.
 */
export interface Store {
  load(): Promise<AgentState | null>;
  save(state: AgentState): Promise<void>;
  lock?(): Promise<() => Promise<void>>;
}

export type LogFields = Record<string, unknown>;

export interface Logger {
  debug(event: string, fields?: LogFields): void;
  info(event: string, fields?: LogFields): void;
  warn(event: string, fields?: LogFields): void;
  error(event: string, fields?: LogFields): void;
}

export type Clock = () => Date;
