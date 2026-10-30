import { setTimeout as sleep } from "node:timers/promises";
import { type ServiceError, status as Code } from "@grpc/grpc-js";
import { dial, type Endpoint, metadataOf, parseEndpoint } from "./endpoint";
import { errorInfo, FinalRevertedError, OutOfRangeError } from "./errors";
import { Finality } from "./gen/stream";
import { type Apply, Lane, Merger, PendingDedup } from "./merge";
import { type Event, open, parseFilter, parseFinality, type Request } from "./session";
import { formatToken, parseToken } from "./token";
import type {
  BlockRef,
  Handler,
  Kind,
  RunOptions,
  StreamConfig,
} from "./types";

/** Ends that stop the run with an error (spec §9.5); any other reconnects. */
const terminal = new Set<number>([
  Code.OUT_OF_RANGE,
  Code.ABORTED,
  Code.UNAUTHENTICATED,
  Code.PERMISSION_DENIED,
  Code.INVALID_ARGUMENT,
]);

const backoffMin = 500;
const backoffMax = 30_000;
/** A connection that lasted this long resets the backoff. */
const backoffReset = 60_000;
/** The heartbeat the watchdog assumes when the server picks it: its cap. */
const serverHeartbeatCap = 30;

const kinds: Kind[] = ["newHeads", "logs", "newPendingTransactions"];

export interface Stream {
  /**
   * Streams until the signal aborts (resolves) or a terminal end (rejects
   * with OutOfRangeError, FinalRevertedError, the gRPC ServiceError, or
   * the handler's own error).
   */
  run(handler: Handler, options?: RunOptions): Promise<void>;
}

/** Validates config and returns a stream to run (spec §9.4). */
export function stream(config: StreamConfig): Stream {
  if (!kinds.includes(config.kind)) {
    throw new TypeError(`kind ${config.kind}: want ${kinds.join(", ")}`);
  }
  if (config.endpoints.length < 1 || config.endpoints.length > 2) {
    throw new TypeError("endpoints: 1, or 2 to merge");
  }
  const endpoints = config.endpoints.map(parseEndpoint);
  const pending = config.kind === "newPendingTransactions";
  if (pending && (config.finality || config.cursor || config.filter)) {
    throw new TypeError("newPendingTransactions takes no finality, cursor or filter");
  }
  if (!pending && config.fullTransactions) {
    throw new TypeError("fullTransactions is for newPendingTransactions only");
  }
  if (config.kind !== "logs" && config.filter) {
    throw new TypeError("filter is for logs only");
  }
  const hb = config.heartbeatSeconds ?? 0;
  if (!Number.isInteger(hb) || hb < 0 || hb > 30) {
    throw new TypeError("heartbeatSeconds: 0 (server default) or 1–30");
  }
  const req: Request = {
    kind: config.kind,
    ...parseFinality(config.finality),
    filter: config.kind === "logs" ? parseFilter(config.filter ?? {}) : undefined,
    heartbeatSeconds: hb,
    fullTransactions: config.fullTransactions ?? false,
  };
  return {
    run: (handler, options) => run(config, endpoints, req, handler, options?.signal),
  };
}

async function run(
  config: StreamConfig,
  endpoints: Endpoint[],
  req: Request,
  handler: Handler,
  signal: AbortSignal | undefined,
): Promise<void> {
  const pending = req.kind === "newPendingTransactions";
  if (!pending && req.finality !== Finality.FINALITY_FINALIZED && !handler.undo) {
    throw new TypeError("handler.undo is required unless finality is finalized");
  }
  const cursor = config.cursor;
  const saved = cursor && (await cursor.load());
  const app: Apply = {
    apply: async (b) => {
      await handler.apply(b);
      await cursor?.save(formatToken(b));
    },
    undo: async (fp) => {
      if (!handler.undo) throw new Error(`undo to ${formatToken(fp)}: handler has no undo`);
      await handler.undo(fp);
      await cursor?.save(formatToken(fp));
    },
  };
  const merger = pending ? undefined : new Merger(saved ? parseToken(saved) : undefined, app);
  const dedup = pending ? new PendingDedup() : undefined;

  const stop = new AbortController();
  let failure: { err: unknown } | undefined;
  const fail = (err: unknown) => {
    failure ??= { err };
    stop.abort();
  };
  const onAbort = () => stop.abort();
  if (signal?.aborted) return;
  signal?.addEventListener("abort", onAbort);

  // Events of every connection reach the app one at a time, in order.
  let queue: Promise<unknown> = Promise.resolve();
  const serial = (fn: () => Promise<void>): Promise<void> => {
    const p = queue.then(fn);
    queue = p.catch(() => {});
    return p;
  };

  const handle = async (lane: Lane, ev: Event): Promise<void> => {
    switch (ev.t) {
      case "block":
        if (dedup) {
          const b = dedup.fresh(ev.block);
          if (b) await handler.apply(b);
        } else await merger!.block(lane, ev.block);
        return;
      case "undo":
        await merger!.undo(lane, ev.forkPoint);
        return;
      case "status":
        handler.status?.({ state: ev.state, reason: ev.reason, endpoint: lane.endpoint });
        return;
      case "heartbeat":
        return;
    }
  };

  const watchdog = 3 * (req.heartbeatSeconds || serverHeartbeatCap) * 1000;

  const connect = async (e: Endpoint): Promise<void> => {
    let restart: (() => void) | undefined;
    const lane = new Lane(e.url, () => restart?.());
    merger?.lanes.push(lane);
    let backoff = backoffMin;
    const md = metadataOf(e, config.metadata);
    while (!stop.signal.aborted) {
      merger?.connected(lane);
      const from: BlockRef | undefined = merger?.cursor;
      const client = dial(e);
      const { call, events } = open(client, md, req, from);
      let why: "watchdog" | "restart" | undefined;
      restart = () => {
        why ??= "restart";
        call.cancel();
      };
      const cancel = () => call.cancel();
      stop.signal.addEventListener("abort", cancel);
      const started = Date.now();
      let flowed = false;
      let err: unknown;
      try {
        for (;;) {
          const timer = setTimeout(() => {
            why ??= "watchdog";
            call.cancel();
          }, watchdog);
          let r: IteratorResult<Event>;
          try {
            r = await events.next();
          } finally {
            clearTimeout(timer);
          }
          if (r.done) break;
          flowed = true;
          try {
            await serial(() => handle(lane, r.value));
          } catch (appErr) {
            fail(appErr);
            break;
          }
        }
      } catch (e) {
        err = e;
      } finally {
        stop.signal.removeEventListener("abort", cancel);
        restart = undefined;
        call.cancel();
        client.close();
      }
      if (stop.signal.aborted) return;
      const se = why ? undefined : (err as ServiceError | undefined);
      if (se && typeof se.code !== "number") {
        fail(err);
        return;
      }
      if (se && terminal.has(se.code)) {
        fail(terminalError(se, merger?.cursor));
        return;
      }
      if (Date.now() - started >= backoffReset) backoff = backoffMin;
      const reason = why ?? (se ? Code[se.code] : "ended");
      handler.status?.({ state: "reconnecting", reason, endpoint: e.url });
      const atOnce =
        why === "restart" ||
        (flowed && (why === "watchdog" || !se || se.code === Code.UNAVAILABLE));
      if (!atOnce) {
        const jittered = backoff / 2 + Math.random() * (backoff / 2);
        await sleep(jittered, undefined, { signal: stop.signal }).catch(() => {});
        backoff = Math.min(backoff * 2, backoffMax);
      }
    }
  };

  try {
    await Promise.all(endpoints.map(connect));
  } finally {
    signal?.removeEventListener("abort", onAbort);
  }
  await queue;
  if (failure) throw failure.err;
}

function terminalError(err: ServiceError, cursor: BlockRef | undefined): Error {
  switch (err.code) {
    case Code.OUT_OF_RANGE: {
      const info = errorInfo(err);
      const block = info?.metadata.oldestBlock;
      const hash = info?.metadata.oldestHash;
      return new OutOfRangeError(
        cursor && formatToken(cursor),
        info?.reason ?? "",
        block && hash ? { number: BigInt(block), hash: hash.toLowerCase() } : undefined,
      );
    }
    case Code.ABORTED:
      return new FinalRevertedError(err.details);
    default:
      return err;
  }
}
