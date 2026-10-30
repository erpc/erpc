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

const backoffMin = 500;
const backoffMax = 30_000;
/** A connection that lasted this long resets the backoff. */
const backoffReset = 60_000;
/**
 * The heartbeat asked for when the config leaves it 0 (spec §9.5): the
 * server's lowest default. The library always sends one, so its watchdog
 * runs on the server's actual interval.
 */
const defaultHeartbeatSeconds = 5;

const kinds: Kind[] = ["newHeads", "logs", "newPendingTransactions"];

export interface Stream {
  /**
   * Streams until the signal aborts (resolves) or a terminal end (rejects
   * with the handler's own error, the gRPC ServiceError for auth or
   * options, or OutOfRangeError or FinalRevertedError from an endpoint
   * while no other endpoint streams).
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
    throw new TypeError("heartbeatSeconds: 0 (5) or 1–30");
  }
  const req: Request = {
    kind: config.kind,
    ...parseFinality(config.finality),
    filter: config.kind === "logs" ? parseFilter(config.filter ?? {}) : undefined,
    heartbeatSeconds: hb || defaultHeartbeatSeconds,
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
  const undo = handler.undo?.bind(handler);
  const app: Apply = {
    apply: async (b) => {
      await handler.apply(b);
      await cursor?.save(formatToken(b));
    },
    undo: undo && (async (fp) => {
      await undo(fp);
      await cursor?.save(formatToken(fp));
    }),
  };
  const stop = new AbortController();
  const lanes = endpoints.map((e) => new Lane(e.url));
  const merger = pending
    ? undefined
    : new Merger(lanes, saved ? parseToken(saved) : undefined, app, stop.signal);
  const dedup = pending ? new PendingDedup() : undefined;

  let failure: { err: unknown } | undefined;
  const fail = (err: unknown) => {
    failure ??= { err };
    stop.abort();
  };
  const onAbort = () => stop.abort();
  if (signal?.aborted) return;
  signal?.addEventListener("abort", onAbort);

  // Every lane's events reach the app one at a time, in order. None runs
  // once the run stopped, and an error stops it before the next one runs.
  let queue: Promise<void> = Promise.resolve();
  const serial = (fn: () => Promise<void> | void): Promise<void> => {
    queue = queue.then(async () => {
      if (stop.signal.aborted) return;
      try {
        await fn();
      } catch (err) {
        fail(err);
      }
    });
    return queue;
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

  const watchdog = 3 * req.heartbeatSeconds * 1000;

  const connect = async (e: Endpoint, lane: Lane): Promise<void> => {
    let backoff = backoffMin;
    const md = metadataOf(e, config.metadata);
    while (!stop.signal.aborted) {
      let from: BlockRef | undefined;
      let sess = 0;
      await serial(() => {
        from = merger?.cursor;
        sess = lane.open(from);
      });
      if (stop.signal.aborted) return;
      const client = dial(e);
      const { call, events } = open(client, md, req, from);
      let why: "watchdog" | "restart" | undefined;
      lane.cancel = () => {
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
          // A cancelled call still yields what it buffered: drop it.
          if (r.done || why || stop.signal.aborted) break;
          flowed = true;
          const ev = r.value;
          await serial(() => (lane.live && lane.sess === sess ? handle(lane, ev) : undefined));
        }
      } catch (e) {
        err = e;
      } finally {
        stop.signal.removeEventListener("abort", cancel);
        lane.cancel = () => {};
        call.cancel();
        client.close();
      }
      if (stop.signal.aborted) return;
      const se = why ? undefined : (err as ServiceError | undefined);
      if (se && typeof se.code !== "number") {
        fail(err);
        return;
      }
      const end = se && terminal(se, e.url, from);
      const reason = why ?? (se ? Code[se.code] : "ended");
      await serial(async () => {
        lane.live = false;
        if (end && (!end.perEndpoint || !lanes.some((l) => l.live))) {
          fail(end.err);
          return;
        }
        handler.status?.({ state: "reconnecting", reason, endpoint: e.url });
        await merger?.follow();
      });
      if (stop.signal.aborted) return;
      if (Date.now() - started >= backoffReset) backoff = backoffMin;
      // A session the library restarted backs off as any other, so a
      // server that keeps breaking the chain linkage is not dialled in a loop.
      const atOnce =
        !end &&
        why !== "restart" &&
        flowed &&
        (why === "watchdog" || !se || se.code === Code.UNAVAILABLE);
      if (!atOnce) {
        const jittered = backoff / 2 + Math.random() * (backoff / 2);
        await sleep(jittered, undefined, { signal: stop.signal }).catch(() => {});
        backoff = Math.min(backoff * 2, backoffMax);
      }
    }
  };

  try {
    await Promise.all(endpoints.map((e, i) => connect(e, lanes[i])));
  } finally {
    signal?.removeEventListener("abort", onAbort);
  }
  await queue;
  if (failure) throw failure.err;
}

/**
 * The error a session's end stops the run with (spec §9.5), undefined
 * when it reconnects. perEndpoint: the error is this endpoint's view of
 * the chain, so it stops the run only while no other endpoint streams.
 * from is the cursor the session resumed from.
 */
function terminal(
  err: ServiceError,
  endpoint: string,
  from: BlockRef | undefined,
): { err: Error; perEndpoint: boolean } | undefined {
  switch (err.code) {
    case Code.OUT_OF_RANGE: {
      const info = errorInfo(err);
      const block = info?.metadata.oldestBlock;
      const hash = info?.metadata.oldestHash;
      return {
        err: new OutOfRangeError(
          endpoint,
          from && formatToken(from),
          info?.reason ?? err.details,
          block && hash ? { number: BigInt(block), hash: hash.toLowerCase() } : undefined,
        ),
        perEndpoint: true,
      };
    }
    case Code.ABORTED:
      return { err: new FinalRevertedError(endpoint, err.details), perEndpoint: true };
    case Code.UNAUTHENTICATED:
    case Code.PERMISSION_DENIED:
    case Code.INVALID_ARGUMENT:
      return { err, perEndpoint: false };
  }
  return undefined;
}
