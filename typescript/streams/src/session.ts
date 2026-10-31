import type { ClientReadableStream, Metadata } from "@grpc/grpc-js";
import type { BlockHeader, Log } from "./gen/models";
import {
  Finality,
  Reorg,
  type StreamBlocksResponse,
  type StreamLogsResponse,
  type StreamPendingTransactionsResponse,
  type StreamServiceClient,
  type StreamStatus,
  StreamStatus_State,
} from "./gen/stream";
import type { LogFilter } from "./gen/query";
import { bytes, hex } from "./token";
import type {
  Block,
  BlockRef,
  Filter,
  Finality as FinalityOption,
  Kind,
  StatusState,
} from "./types";

/** One stream message as the run loop reads it, whatever the kind. */
export type Event =
  | { t: "block"; block: Block }
  | { t: "undo"; forkPoint: BlockRef }
  | { t: "heartbeat" }
  | { t: "status"; state: StatusState; reason: string };

export interface Request {
  kind: Kind;
  finality: Finality;
  depth: number;
  filter: LogFilter | undefined;
  heartbeatSeconds: number;
  fullTransactions: boolean;
}

/** f as the proto takes it; addresses 20 bytes, topics 32. */
export function parseFilter(f: Filter): LogFilter {
  const hexOf = (h: string, n: number, what: string) => {
    const b = bytes(h);
    if (b.length !== n) throw new TypeError(`filter ${what} ${h}: want ${n} bytes`);
    return b;
  };
  const list = (v: string | string[] | null | undefined) =>
    v == null ? [] : Array.isArray(v) ? v : [v];
  return {
    address: list(f.address).map((a) => hexOf(a, 20, "address")),
    topics: (f.topics ?? []).map((p) => ({ values: list(p).map((t) => hexOf(t, 32, "topic")) })),
  };
}

export function parseFinality(f: FinalityOption | undefined): {
  finality: Finality;
  depth: number;
} {
  switch (f ?? "latest") {
    case "latest":
      return { finality: Finality.FINALITY_LATEST, depth: 0 };
    case "safe":
      return { finality: Finality.FINALITY_SAFE, depth: 0 };
    case "finalized":
      return { finality: Finality.FINALITY_FINALIZED, depth: 0 };
  }
  const m = /^depth:([1-9][0-9]*)$/.exec(f as string);
  if (!m) throw new TypeError(`finality ${f}: want latest, safe, finalized or depth:N`);
  return { finality: Finality.FINALITY_DEPTH, depth: Number(m[1]) };
}

const states: Record<StreamStatus_State, StatusState | undefined> = {
  [StreamStatus_State.STATE_OK]: "ok",
  [StreamStatus_State.STATE_DEGRADED]: "degraded",
  [StreamStatus_State.STATE_LAGGING]: "lagging",
  [StreamStatus_State.UNRECOGNIZED]: undefined,
};

function status(s: StreamStatus): Event {
  return { t: "status", state: states[s.state] ?? "degraded", reason: s.reason };
}

function emptyBlock(number: bigint, hash: string, parentHash: string): Block {
  return { number, hash, parentHash, logs: [], hashes: [], transactions: [] };
}

/**
 * Opens one stream of req's kind after from and yields its events: a
 * logs block once its `complete` message arrived; a StreamBlocks resend
 * from the fork point + 1 as an undo to the fork point, then the block.
 */
export function open(
  client: StreamServiceClient,
  md: Metadata,
  req: Request,
  from: BlockRef | undefined,
): { call: ClientReadableStream<unknown>; events: AsyncGenerator<Event> } {
  const resume = from
    ? { fromBlock: from.number, fromHash: bytes(from.hash) }
    : {};
  switch (req.kind) {
    case "newHeads": {
      const call = client.streamBlocks(
        {
          ...resume,
          finality: req.finality,
          depth: req.depth,
          heartbeatSeconds: req.heartbeatSeconds,
        },
        md,
      );
      return { call, events: heads(call, from?.number) };
    }
    case "logs": {
      const call = client.streamLogs(
        {
          ...resume,
          filter: req.filter,
          finality: req.finality,
          depth: req.depth,
          reorg:
            req.finality === Finality.FINALITY_FINALIZED
              ? Reorg.REORG_NONE
              : Reorg.REORG_UNDO,
          heartbeatSeconds: req.heartbeatSeconds,
        },
        md,
      );
      return { call, events: logs(call) };
    }
    case "newPendingTransactions": {
      const call = client.streamPendingTransactions(
        {
          fullTransactions: req.fullTransactions,
          heartbeatSeconds: req.heartbeatSeconds,
        },
        md,
      );
      return { call, events: pending(call) };
    }
  }
}

async function* heads(
  call: ClientReadableStream<StreamBlocksResponse>,
  last: bigint | undefined,
): AsyncGenerator<Event> {
  for await (const m of call as AsyncIterable<StreamBlocksResponse>) {
    switch (m.message?.$case) {
      case "header": {
        const h: BlockHeader = m.message.value;
        const parentHash = hex(h.parentHash);
        if (last !== undefined && h.number <= last) {
          yield { t: "undo", forkPoint: { number: h.number - 1n, hash: parentHash } };
        }
        last = h.number;
        yield {
          t: "block",
          block: { ...emptyBlock(h.number, hex(h.hash), parentHash), header: h },
        };
        break;
      }
      case "heartbeat":
        yield { t: "heartbeat" };
        break;
      case "status":
        yield status(m.message.value);
        break;
    }
  }
}

async function* logs(
  call: ClientReadableStream<StreamLogsResponse>,
): AsyncGenerator<Event> {
  let held: Log[] = [];
  for await (const m of call as AsyncIterable<StreamLogsResponse>) {
    switch (m.message?.$case) {
      case "block": {
        const b = m.message.value;
        held = held.concat(b.logs);
        if (!b.complete || !b.block) break;
        const block = emptyBlock(b.block.number, hex(b.block.hash), hex(b.block.parentHash));
        block.logs = held.map((l) => Object.assign(l, { key: `${hex(l.blockHash)}:${l.logIndex}` }));
        held = [];
        yield { t: "block", block };
        break;
      }
      case "undo": {
        held = [];
        const fp = m.message.value.forkPoint;
        if (fp) yield { t: "undo", forkPoint: { number: fp.number, hash: hex(fp.hash) } };
        break;
      }
      case "heartbeat":
        yield { t: "heartbeat" };
        break;
      case "status":
        yield status(m.message.value);
        break;
    }
  }
}

async function* pending(
  call: ClientReadableStream<StreamPendingTransactionsResponse>,
): AsyncGenerator<Event> {
  for await (const m of call as AsyncIterable<StreamPendingTransactionsResponse>) {
    switch (m.message?.$case) {
      case "transactions": {
        const block = emptyBlock(0n, "", "");
        block.hashes = m.message.value.hashes.map(hex);
        block.transactions = m.message.value.transactions;
        yield { t: "block", block };
        break;
      }
      case "heartbeat":
        yield { t: "heartbeat" };
        break;
      case "status":
        yield status(m.message.value);
        break;
    }
  }
}
