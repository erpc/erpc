import type { BlockHeader, Log, Transaction } from "./gen/types/models";

export type { BlockHeader, Log, Transaction };

/** What a stream delivers. */
export type Kind = "newHeads" | "logs" | "newPendingTransactions";

/** The highest block a stream applies (spec §3.1). */
export type Finality = "latest" | "safe" | "finalized" | `depth:${number}`;

/**
 * A logs filter with eth_subscribe semantics: hex addresses, and per topic
 * position the values any of which matches; null or empty matches any.
 */
export interface Filter {
  address?: string | string[];
  topics?: (string | string[] | null)[];
}

/**
 * Where the resume token lives: the cursor of the last block the app
 * applied, saved after each apply or undo returns.
 */
export interface Cursor {
  load(): Promise<string | undefined> | string | undefined;
  save(token: string): Promise<void> | void;
}

export interface StreamConfig {
  /** 1 endpoint, or 2 merged: `grpc(s)://host:port/<project>/evm/<chainId>`. */
  endpoints: string[];
  kind: Kind;
  /** Logs only. */
  filter?: Filter;
  /** Heads and logs; default "latest". */
  finality?: Finality;
  /** Heads and logs; without one every run starts after the bound. */
  cursor?: Cursor;
  /** Sent on every stream, e.g. `x-erpc-secret-token`. */
  metadata?: Record<string, string>;
  /** 1–30; 0 or unset is the server default. */
  heartbeatSeconds?: number;
  /** Pending only: full transactions instead of hashes. */
  fullTransactions?: boolean;
}

export interface BlockRef {
  number: bigint;
  /** 0x-prefixed lowercase hex. */
  hash: string;
}

/** A log with its dedup key, `0x<blockHash>:<logIndex>`. */
export type StreamLog = Log & { key: string };

/**
 * One applied unit: a block's header (heads), its matched logs (logs), or a
 * batch of pending transactions (number 0, empty hashes).
 */
export interface Block {
  number: bigint;
  hash: string;
  parentHash: string;
  header?: BlockHeader;
  logs: StreamLog[];
  /** Pending hashes, 0x-prefixed lowercase hex. */
  hashes: string[];
  transactions: Transaction[];
}

export type StatusState = "ok" | "degraded" | "lagging" | "reconnecting";

export interface Status {
  state: StatusState;
  /** The server's reason, or for reconnecting the gRPC code or "watchdog". */
  reason: string;
  endpoint: string;
}

export interface Handler {
  /** Idempotent by `log.key` (logs) or `hash`: delivery is at-least-once. */
  apply(block: Block): Promise<void> | void;
  /** Drop state above `forkPoint.number`. Required unless finality is finalized. */
  undo?(forkPoint: BlockRef): Promise<void> | void;
  status?(status: Status): void;
}

export interface RunOptions {
  /** Aborting stops the run; `run` then resolves. */
  signal?: AbortSignal;
}
