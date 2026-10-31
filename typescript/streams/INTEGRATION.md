# Streams integration

> Source: https://docs.erpc.cloud/operation/streams-integration
> Integrate eRPC block, log and mempool streams into a backend app over gRPC or ws, gap-free across reconnects, reorgs and instances.
> Format: machine-readable markdown export of the docs page above.
> All collapsible AI sections are inlined and fully expanded.

# Streams integration

This page tells a backend app, or the coding agent that writes it, how to consume eRPC streams of new heads, logs and pending transactions with no gap and no silent loss: across reconnects, reorgs, instance restarts and two regions. Every code block is a complete file that eRPC's test suite builds and runs against a local eRPC.

Both Go and Node libraries ship this page as `INTEGRATION.md`.

## 1. Choose

| You have | Use | Section |
|---|---|---|
| a Go or Node (≥ 22) backend | the library: `github.com/erpc/erpc/sdk/go/streams` or `@erpc-cloud/streams`, over gRPC | [2](#2-go-and-node-libraries) |
| Python, Rust or Java | the generated `bds` gRPC stubs, with the library's rules done by hand | [3](#3-python-rust-and-java) |
| a client that must stay on WebSocket | ws extended subscriptions | [4](#4-ws-extended) |
| any of the above, but only need "something new happened" | standard `eth_subscribe` ([WebSocket](https://docs.erpc.cloud/operation/websocket.llms.txt)): lossy on restarts, no resume | — |

The server needs `server.grpcEnabled: true` for gRPC ([gRPC & BDS streaming](https://docs.erpc.cloud/reference/grpc-bds.llms.txt)); it serves gRPC on the HTTP port unless `server.grpcPortV4` is set. ws needs `server.webSocket.enabled: true`. An endpoint is `grpc://host:port/<project>/evm/<chainId>` (`grpcs://` with TLS); the client sends the path as the `x-erpc-project` and `x-erpc-chain-id` metadata, and an auth secret as `x-erpc-secret-token`.

Pick the finality: the highest block the stream delivers.

| Finality | Delivers up to | Reorgs reaching the app | Latency | Reorg handling |
|---|---|---|---|---|
| `latest` | the confirmed head | every reorg | lowest | `undo(forkPoint)`: drop state above `forkPoint.number` |
| `depth:N` | confirmed head − N (N 1 to the server's `chainTracker.reorgWindow`) | only those deeper than N | N blocks | `undo`, rarely |
| `safe` | the chain's `safe` block | rare | chain-dependent | `undo`, rarely |
| `finalized` | the chain's `finalized` block | none: a majority revert of a delivered final block ends the stream `final_reverted` | about 13 min on Ethereum | none: no `undo`; rebuild state from before the reverted block on `FinalRevertedError` |

The guarantee is the same everywhere: blocks arrive oldest first, each links to the one before by `parentHash`, each once per branch; every break is an `undo` to a block the app applied, or an end the app sees. Delivery is at least once: the app may see a block again after a crash, so applying must be idempotent.

| Kind | Dedup key, stable across instances and reconnects |
|---|---|
| logs | `(blockHash, logIndex)`; the libraries' `Log.Key()` / `log.key` is `<blockHash>:<logIndex>` |
| heads | block `hash` |
| pending | transaction `hash` |

## 2. Go and Node libraries

Install with `go get github.com/erpc/erpc/sdk/go/streams` (Go ≥ 1.24) or `npm install @erpc-cloud/streams` (Node ≥ 22). Both have one API: a config, then `apply`, `undo` and `status` callbacks run one at a time, in chain order.

**Config.** `endpoints`: 1, or 2 to merge. `kind`: `newHeads`, `logs` or `newPendingTransactions`. `filter` (logs): `address` and `topics` as `eth_getLogs`. `finality`: as above, default `latest`. `cursor`: where the resume token lives. `metadata`: sent on every stream. `heartbeatSeconds`: 1–30, default the server's (the chain's block time clamped to 5–30s).

**Cursor.** The token is `<numberHex>:<hash>` of the last block the app applied, or of the fork point after an `undo`. The library saves it after `apply` or `undo` returns without error, and resumes from it on every reconnect and restart, on any instance. `FileCursor(path)` / `fileCursor(path)` replace a file atomically. To keep state and cursor consistent in a database, implement the two methods (`Load`/`Save`, `load`/`save`) over the same database; with an idempotent `apply` a crash between the two only repeats a block. Without a cursor every run starts after the bound at subscribe.

**Apply.** Called once per block (logs: every block, also with no matching log; pending: once per batch). Write by the dedup key (upsert), never append, so a repeat changes nothing. An error from `apply` or `undo` ends the run with it, cursor unchanged.

**Undo.** Called with the fork point before the new branch: delete everything the app derived from blocks above `forkPoint.number`. Required for every finality but `finalized`, where the library asks for no reorgs.

**Status.** `ok`, `degraded` (no fresh source on that instance), `lagging` (the stream fell behind), `reconnecting` (with the gRPC code, or `watchdog`). Informational: the library acts on its own.

**What the library does for you:**

- **Windows**: dials with the server's flow-control windows, 4 MiB per stream and 16 MiB per connection.
- **Heartbeat watchdog**: no message for 3 heartbeats (a black-holed link) cancels the stream and reconnects at once.
- **Reconnect**: `UNAVAILABLE` after messages flowed (instance shutdown, the 10-minute connection age, a stall, lag) resumes at once from the cursor; any other failure backs off with jitter from 0.5s to 30s, reset after 60s connected.
- **Dedup**: a block whose hash the app already applied on the current branch is skipped.
- **Merge**: with two endpoints (two instances or regions), both stream from the merged cursor; the library follows the one with the highest block, a tie keeps the current one. Switching to the other's branch calls `undo` to the fork point of the two chains, then `apply` for that endpoint's blocks above it; a block already applied is skipped. Either endpoint can die and the app keeps its blocks.
- **Terminal ends**: the run returns (Go) or rejects (Node) only on stop, `OutOfRangeError`, `FinalRevertedError`, an auth or option error (`UNAUTHENTICATED`, `PERMISSION_DENIED`, `INVALID_ARGUMENT`), or a callback's error.

**Out of range.** The server holds a window of recent blocks. An app away longer than that gets `OutOfRangeError` with `Cursor` (the last block applied) and `Oldest` (the oldest block a stream can resume from); nothing past the gap is applied. Neither eRPC nor the library backfills older than the window: fetch the blocks after `Cursor` through `Oldest` with `eth_getLogs` (or `eth_getBlockByNumber`) over HTTP, save `Oldest`'s token as the cursor, then run again. Do it at once: the window moves.

### Go

Logs at `latest`, applied by key, undone above the fork point, stopped on out of range:

```go filename="sdk/go/streams/examples/logs/main.go"
// Logs streams a contract's logs into a map keyed by Log.Key, resuming
// from erpc.cursor, and exits after BLOCKS blocks (default: never).
//
//	ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… go run ./examples/logs
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"

	"github.com/erpc/erpc/sdk/go/streams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	limit, _ := strconv.Atoi(os.Getenv("BLOCKS"))

	held := map[string]uint64{} // log key → block number
	blocks := 0
	var filter streams.Filter
	if a := os.Getenv("ERPC_ADDRESS"); a != "" {
		filter.Address = []string{a}
	}
	md := map[string]string{}
	if t := os.Getenv("ERPC_TOKEN"); t != "" {
		md["x-erpc-secret-token"] = t
	}
	err := streams.Run(ctx, streams.Config{
		Endpoints: []string{os.Getenv("ERPC_ENDPOINT")},
		Kind:      streams.Logs,
		Filter:    filter,
		Finality:  streams.Latest,
		Cursor:    streams.FileCursor("erpc.cursor"),
		Metadata:  md,
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			for _, l := range b.Logs {
				held[l.Key()] = b.Number
			}
			fmt.Printf("block %d %s: %d logs\n", b.Number, b.Hash, len(b.Logs))
			if blocks++; blocks == limit {
				stop()
			}
			return nil
		},
		Undo: func(ctx context.Context, fp streams.BlockRef) error {
			for k, n := range held {
				if n > fp.Number {
					delete(held, k)
				}
			}
			fmt.Printf("undo to %d %s\n", fp.Number, fp.Hash)
			return nil
		},
		Status: func(s streams.Status) { fmt.Printf("status %s %s %s\n", s.Endpoint, s.State, s.Reason) },
	})
	var oor *streams.OutOfRangeError
	if errors.As(err, &oor) {
		// Away longer than the server's window. Backfill the blocks after
		// oor.Cursor through oor.Oldest with eth_getLogs, save oor.Oldest.Token()
		// to erpc.cursor, then start again.
		oldest := "unknown"
		if oor.Oldest != nil {
			oldest = oor.Oldest.Token()
		}
		log.Fatalf("cursor %s is out of range (%s); oldest resumable %s", oor.Cursor, oor.Reason, oldest)
	}
	if err != nil {
		log.Fatal(err)
	}
}
```

New heads at `finalized` from two endpoints merged into one chain (no `Undo`):

```go filename="sdk/go/streams/examples/merge/main.go"
// Merge follows new heads from two eRPC endpoints merged into one chain,
// finalized only, and exits after BLOCKS blocks (default: never).
//
//	ERPC_ENDPOINTS=grpc://a:4001/main/evm/1,grpc://b:4001/main/evm/1 go run ./examples/merge
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/erpc/erpc/sdk/go/streams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	limit, _ := strconv.Atoi(os.Getenv("BLOCKS"))
	blocks := 0
	err := streams.Run(ctx, streams.Config{
		Endpoints: strings.Split(os.Getenv("ERPC_ENDPOINTS"), ","),
		Kind:      streams.NewHeads,
		Finality:  streams.Finalized,
		Cursor:    streams.FileCursor("heads.cursor"),
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			fmt.Printf("final block %d %s at %d\n", b.Number, b.Hash, b.Header.Timestamp)
			if blocks++; blocks == limit {
				stop()
			}
			return nil
		},
		Status: func(s streams.Status) { fmt.Printf("status %s %s %s\n", s.Endpoint, s.State, s.Reason) },
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

Pending transaction hashes (no cursor, no finality, no `Undo`):

```go filename="sdk/go/streams/examples/pending/main.go"
// Pending prints pending transaction hashes and exits after BATCHES
// batches (default: never).
//
//	ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 go run ./examples/pending
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"

	"github.com/erpc/erpc/sdk/go/streams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	limit, _ := strconv.Atoi(os.Getenv("BATCHES"))
	batches := 0
	err := streams.Run(ctx, streams.Config{
		Endpoints: []string{os.Getenv("ERPC_ENDPOINT")},
		Kind:      streams.NewPendingTransactions,
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			for _, h := range b.Hashes {
				fmt.Println(h)
			}
			if batches++; batches == limit {
				stop()
			}
			return nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

### Node

Logs at `latest`; `run` resolves when its signal aborts:

```ts filename="typescript/streams/examples/logs.mts"
// Logs into a map keyed by log.key, applied once per branch, resumed from
// erpc.cursor after every restart; exits after BLOCKS blocks (default: never).
//
//   ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… node examples/logs.mts
import { fileCursor, OutOfRangeError, stream } from "@erpc-cloud/streams";

const limit = Number(process.env.BLOCKS ?? 0);
const held = new Map<string, bigint>(); // log key → block number
let blocks = 0;

const s = stream({
  endpoints: [process.env.ERPC_ENDPOINT ?? "grpcs://erpc-a.example:443/main/evm/1"],
  kind: "logs",
  filter: process.env.ERPC_ADDRESS ? { address: [process.env.ERPC_ADDRESS] } : {},
  finality: "latest",
  cursor: fileCursor("./erpc.cursor"),
  metadata: process.env.ERPC_TOKEN ? { "x-erpc-secret-token": process.env.ERPC_TOKEN } : {},
});

const ac = new AbortController();
process.on("SIGINT", () => ac.abort());

try {
  await s.run(
    {
      apply: (block) => {
        for (const log of block.logs) held.set(log.key, block.number);
        console.log(`block ${block.number} ${block.hash}: ${block.logs.length} logs`);
        if (++blocks === limit) ac.abort();
      },
      undo: (forkPoint) => {
        for (const [key, n] of held) if (n > forkPoint.number) held.delete(key);
        console.log(`undo to ${forkPoint.number} ${forkPoint.hash}`);
      },
      status: (st) => console.log(`status ${st.endpoint} ${st.state} ${st.reason}`),
    },
    { signal: ac.signal },
  );
} catch (err) {
  if (err instanceof OutOfRangeError) {
    // Away longer than the server's window. Backfill the blocks after
    // err.cursor through err.oldest with eth_getLogs, save err.oldest's
    // token to erpc.cursor, then start again.
    console.error(err.message);
    process.exit(2);
  }
  throw err;
}
```

New heads at `depth:3` from two endpoints merged:

```ts filename="typescript/streams/examples/merge.mts"
// New heads from two eRPC endpoints merged into one chain, 3 blocks below
// the head: either endpoint may go away and the app keeps its blocks,
// undoing only on a real branch switch. Exits after BLOCKS blocks
// (default: never).
//
//   ERPC_ENDPOINTS=grpc://a:4001/main/evm/8453,grpc://b:4001/main/evm/8453 node examples/merge.mts
import { stream } from "@erpc-cloud/streams";

const limit = Number(process.env.BLOCKS ?? 0);
let blocks = 0;

const s = stream({
  endpoints: (
    process.env.ERPC_ENDPOINTS ?? "grpcs://erpc-a.example:443/main/evm/8453,grpcs://erpc-b.example:443/main/evm/8453"
  ).split(","),
  kind: "newHeads",
  finality: "depth:3",
});

const ac = new AbortController();
process.on("SIGINT", () => ac.abort());

await s.run(
  {
    apply: (block) => {
      console.log(`block ${block.number} ${block.hash} at ${block.header?.timestamp}`);
      if (++blocks === limit) ac.abort();
    },
    undo: (forkPoint) => console.log(`undo to ${forkPoint.number} ${forkPoint.hash}`),
    status: (st) => console.log(`status ${st.endpoint} ${st.state} ${st.reason}`),
  },
  { signal: ac.signal },
);
```

Pending transaction hashes:

```ts filename="typescript/streams/examples/pending.mts"
// Pending transaction hashes; exits after BATCHES batches (default: never).
//
//   ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 node examples/pending.mts
import { stream } from "@erpc-cloud/streams";

const limit = Number(process.env.BATCHES ?? 0);
let batches = 0;

const s = stream({
  endpoints: [process.env.ERPC_ENDPOINT ?? "grpcs://erpc-a.example:443/main/evm/1"],
  kind: "newPendingTransactions",
});

const ac = new AbortController();
process.on("SIGINT", () => ac.abort());

await s.run(
  {
    apply: (batch) => {
      for (const hash of batch.hashes) console.log(hash);
      if (++batches === limit) ac.abort();
    },
  },
  { signal: ac.signal },
);
```

## 3. Python, Rust and Java

The `bds` packages of the [Blockchain Data Standards manifesto](https://github.com/blockchain-data-standards/manifesto) hold the generated `StreamService` stubs. Install:

- Python: `pip install grpcio grpcio-status "bds @ git+https://github.com/blockchain-data-standards/manifesto#subdirectory=python"`
- Rust: the `bds` git dependency in the `Cargo.toml` below; it needs no `protoc`.
- Java: `mvn -f java/pom.xml install` in a manifesto checkout, then the `io.github.blockchain-data-standards:bds:0.1.0` dependency below.

Use `StreamLogs`, `StreamBlocks` (heads) or `StreamPendingTransactions`. Implement these rules by hand, as the examples do for `StreamLogs`:

1. **Metadata**: `x-erpc-project` and `x-erpc-chain-id` from the endpoint path, `x-erpc-secret-token` if the project needs auth.
2. **Windows**: dial with a 4 MiB stream window and a 16 MiB connection window (the default 64 KiB makes log bursts wait on the client). A fresh channel per session: a connection on a black-holed link is never reused.
3. **Request**: `heartbeatSeconds` set (`StreamBlocks` sends heartbeats and status only when it is); `fromBlock` + `fromHash` = the cursor, both or neither; `reorg` `REORG_UNDO`, or `REORG_NONE` with `FINALITY_FINALIZED`; `depth` with `FINALITY_DEPTH`.
4. **Logs**: one `BlockLogs` per block, empty blocks included; a block of more than 1,000 logs spans messages; apply the block at the message with `complete`, then save its cursor. A stream that ends mid-block sends the whole block again on resume.
5. **Heads**: `StreamBlocks` has no `Undo`: after a reorg it sends the new branch from fork point + 1. A header whose `parentHash` is not your last applied hash means: drop state above `number − 1`, then apply it.
6. **Undo** (`StreamLogs`): drop state above `forkPoint.number`, save the fork point as the cursor.
7. **Dedup** (`StreamLogs`): skip a block whose hash equals the cursor's; a block whose `parentHash` is not the cursor's hash breaks linkage: resume from the cursor.
8. **Watchdog**: no message for 3 × `heartbeatSeconds` → cancel, reconnect at once.
9. **Reconnect**: `UNAVAILABLE` (or a server close) after messages flowed → resume at once; else jittered backoff 0.5s → 30s, reset after 60s connected.
10. **Terminal**: `OUT_OF_RANGE` (read `google.rpc.ErrorInfo`: `reason`, metadata `oldestBlock` decimal and `oldestHash`), `ABORTED` (`final_reverted`), `UNAUTHENTICATED`, `PERMISSION_DENIED`, `INVALID_ARGUMENT`: stop and surface it. Out of range: backfill as in section 2.
11. **Merge**: two endpoints by hand is the library's hardest part; run one endpoint per process behind a load balancer, or use a library.

### Python

```python filename="docs/examples/streams/python/stream_logs.py"
"""Logs from eRPC's StreamLogs on the bds stubs, applied to a store keyed by
(blockHash, logIndex) and resumed from erpc.cursor, with the toolkit rules
done by hand: window, heartbeat watchdog, reconnect, undo, out of range.
Exits after BLOCKS blocks (default: never).

    pip install grpcio grpcio-status \
      "bds @ git+https://github.com/blockchain-data-standards/manifesto#subdirectory=python"
    ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… python stream_logs.py
"""

import os
import random
import sys
import threading
import time
from urllib.parse import urlparse

import grpc
from google.rpc import error_details_pb2
from grpc_status import rpc_status

from bds.evm import query_pb2, stream_pb2, stream_pb2_grpc

HEARTBEAT = 15  # seconds; the watchdog reconnects after 3 silent ones
CURSOR_PATH = "erpc.cursor"
UNAVAILABLE = grpc.StatusCode.UNAVAILABLE
TERMINAL = {
    grpc.StatusCode.OUT_OF_RANGE,
    grpc.StatusCode.ABORTED,
    grpc.StatusCode.UNAUTHENTICATED,
    grpc.StatusCode.PERMISSION_DENIED,
    grpc.StatusCode.INVALID_ARGUMENT,
}
# The server's 4 MiB stream window: with the 64 KiB default, log bursts
# wait on this client's acknowledgements.
CHANNEL_OPTIONS = [("grpc.http2.lookahead_bytes", 4 << 20)]


def parse_endpoint(raw):
    """grpc(s)://host:port/<project>/evm/<chainId> → target, tls, metadata."""
    u = urlparse(raw)
    if u.scheme not in ("grpc", "grpcs"):
        raise ValueError(f"{raw}: scheme must be grpc or grpcs")
    project, family, chain = u.path.strip("/").split("/")
    if family != "evm":
        raise ValueError(f"{raw}: path must be /<project>/evm/<chainId>")
    port = u.port or (443 if u.scheme == "grpcs" else 80)
    return f"{u.hostname}:{port}", u.scheme == "grpcs", [("x-erpc-project", project), ("x-erpc-chain-id", chain)]


def token(number, block_hash):
    return f"0x{number:x}:0x{block_hash.hex()}"


class Cursor:
    """The last applied block, in a file replaced atomically on each save."""

    def __init__(self, path):
        self.path = path
        self.number, self.hash = None, None
        if os.path.exists(path):
            num, hx = open(path).read().strip().split(":")
            self.number, self.hash = int(num, 16), bytes.fromhex(hx[2:])

    def save(self, number, block_hash):
        tmp = self.path + ".tmp"
        with open(tmp, "w") as f:
            f.write(token(number, block_hash) + "\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        self.number, self.hash = number, block_hash


class Watchdog:
    """Cancels the call once no message arrived for 3 heartbeats."""

    def __init__(self, call, limit):
        self.call, self.limit, self.last, self.fired = call, limit, time.monotonic(), False
        self.stopped = threading.Event()
        threading.Thread(target=self.watch, daemon=True).start()

    def feed(self):
        self.last = time.monotonic()

    def watch(self):
        while not self.stopped.wait(self.limit / 12):
            if time.monotonic() - self.last > self.limit:
                self.fired = True
                self.call.cancel()
                return


class OutOfRange(Exception):
    pass


class App:
    def __init__(self, limit):
        self.cursor = Cursor(CURSOR_PATH)
        self.logs = {}  # (blockHash, logIndex) → (block number, data)
        self.limit, self.blocks = limit, 0

    def apply(self, ref, logs):
        # Idempotent: a block applied again (a crash before the cursor save)
        # writes the same keys.
        for log in logs:
            self.logs[(log.blockHash, log.logIndex)] = (ref.number, log.data)
        self.cursor.save(ref.number, ref.hash)
        self.blocks += 1
        print(f"block {ref.number} 0x{ref.hash.hex()}: {len(logs)} logs")

    def undo(self, fork_point):
        self.logs = {k: v for k, v in self.logs.items() if v[0] <= fork_point.number}
        self.cursor.save(fork_point.number, fork_point.hash)
        print(f"undo to {fork_point.number} 0x{fork_point.hash.hex()}")

    def done(self):
        return self.limit > 0 and self.blocks >= self.limit


def session(app, stub, metadata, address):
    """One stream from the cursor until it ends: (flowed, code, error).
    code is a grpc.StatusCode, "watchdog", "restart" or None (done)."""
    request = stream_pb2.StreamLogsRequest(
        finality=stream_pb2.FINALITY_LATEST,
        reorg=stream_pb2.REORG_UNDO,
        heartbeatSeconds=HEARTBEAT,
    )
    if address:
        request.filter.CopyFrom(query_pb2.LogFilter(address=[bytes.fromhex(address[2:])]))
    if app.cursor.number is not None:
        request.fromBlock, request.fromHash = app.cursor.number, app.cursor.hash
    call = stub.StreamLogs(request, metadata=metadata)
    dog = Watchdog(call, 3 * HEARTBEAT)
    flowed, part = False, None  # part: (BlockRef, logs) of the block in progress
    try:
        for msg in call:
            flowed = True
            dog.feed()
            kind = msg.WhichOneof("message")
            if kind == "block":
                ref = msg.block.block
                if part is None or part[0].hash != ref.hash:
                    part = (ref, [])
                part[1].extend(msg.block.logs)
                if not msg.block.complete:
                    continue
                logs, part = part[1], None
                if ref.hash == app.cursor.hash:
                    continue  # already applied: skip
                if app.cursor.hash is not None and ref.parentHash != app.cursor.hash:
                    call.cancel()
                    return flowed, "restart", None  # linkage broke: resume from the cursor
                app.apply(ref, logs)
            elif kind == "undo":
                part = None
                app.undo(msg.undo.forkPoint)
            elif kind == "status":
                print(f"status {stream_pb2.StreamStatus.State.Name(msg.status.state)} {msg.status.reason}")
            dog.feed()
            if app.done():
                call.cancel()
                return flowed, None, None
        return flowed, UNAVAILABLE, None  # the server closed the stream
    except grpc.RpcError as err:
        if dog.fired:
            print("watchdog: no message within 3 heartbeats")
            return flowed, "watchdog", None
        return flowed, err.code(), err
    finally:
        dog.stopped.set()


def out_of_range(err, cursor):
    status = rpc_status.from_call(err)
    for detail in status.details if status else []:
        info = error_details_pb2.ErrorInfo()
        if detail.Unpack(info):
            oldest = f"0x{int(info.metadata['oldestBlock']):x}:{info.metadata['oldestHash']}"
            return OutOfRange(f"cursor {token(cursor.number, cursor.hash)} out of range ({info.reason}); oldest resumable {oldest}")
    return OutOfRange(f"cursor out of range: {err.details()}")


def run(endpoint, address, limit):
    target, tls, metadata = parse_endpoint(endpoint)
    if os.environ.get("ERPC_TOKEN"):
        metadata.append(("x-erpc-secret-token", os.environ["ERPC_TOKEN"]))
    app, attempt = App(limit), 0
    while True:
        # A fresh channel per session: a black-holed connection is never reused.
        if tls:
            channel = grpc.secure_channel(target, grpc.ssl_channel_credentials(), CHANNEL_OPTIONS)
        else:
            channel = grpc.insecure_channel(target, CHANNEL_OPTIONS)
        began = time.monotonic()
        with channel:
            flowed, code, err = session(app, stream_pb2_grpc.StreamServiceStub(channel), metadata, address)
        if code is None:
            return
        if code == grpc.StatusCode.OUT_OF_RANGE:
            raise out_of_range(err, app.cursor)
        if code in TERMINAL:
            raise err
        if time.monotonic() - began >= 60:
            attempt = 0
        if code == "restart" or (flowed and code in ("watchdog", UNAVAILABLE)):
            continue  # drained, aged out, stalled or silent: the cursor loses nothing
        delay = min(30.0, 0.5 * 2**attempt)
        attempt += 1
        print(f"reconnect after {code}")
        time.sleep(delay / 2 + random.uniform(0, delay / 2))


if __name__ == "__main__":
    try:
        run(os.environ["ERPC_ENDPOINT"], os.environ.get("ERPC_ADDRESS"), int(os.environ.get("BLOCKS", "0")))
    except OutOfRange as e:
        # Away longer than the server's window. Backfill the blocks after the
        # cursor through the oldest resumable block with eth_getLogs, save
        # that block's token to erpc.cursor, then start again.
        sys.exit(str(e))
```

### Rust

```toml filename="docs/examples/streams/rust/Cargo.toml"
[package]
name = "stream-logs"
version = "0.1.0"
edition = "2021"
publish = false

[dependencies]
bds = { git = "https://github.com/blockchain-data-standards/manifesto" }
bytes = "1"
rand = "0.9"
tokio = { version = "1", features = ["macros", "rt-multi-thread", "time"] }
tonic = { version = "0.14", features = ["tls-ring", "tls-native-roots"] }
tonic-types = "0.14"
```

```rust filename="docs/examples/streams/rust/src/main.rs"
//! Logs from eRPC's StreamLogs on the bds crate, applied to a store keyed by
//! (blockHash, logIndex) and resumed from erpc.cursor, with the toolkit rules
//! done by hand: windows, heartbeat watchdog, reconnect, undo, out of range.
//! Exits after BLOCKS blocks (default: never).
//!
//!     ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… cargo run

use std::{collections::HashMap, env, error::Error, fs, io, time::Duration, time::Instant};

use bds::evm::{
    stream_logs_response::Message, stream_service_client::StreamServiceClient, stream_status::State, BlockRef,
    Finality, Log, LogFilter, Reorg, StreamLogsRequest,
};
use bytes::Bytes;
use rand::Rng;
use tonic::{
    transport::{ClientTlsConfig, Endpoint},
    Code, Request, Status,
};
use tonic_types::StatusExt;

/// Seconds; the watchdog reconnects after 3 silent ones.
const HEARTBEAT: u32 = 15;
const CURSOR_PATH: &str = "erpc.cursor";

/// A parsed grpc(s)://host:port/<project>/evm/<chainId>.
struct Target {
    uri: String,
    tls: bool,
    project: String,
    chain: String,
}

fn parse_endpoint(raw: &str) -> Result<Target, String> {
    let bad = |why: &str| format!("{raw}: {why}");
    let (scheme, rest) = raw.split_once("://").ok_or_else(|| bad("no scheme"))?;
    let tls = match scheme {
        "grpc" => false,
        "grpcs" => true,
        _ => return Err(bad("scheme must be grpc or grpcs")),
    };
    let (authority, path) = rest.split_once('/').ok_or_else(|| bad("no path"))?;
    let parts: Vec<&str> = path.trim_matches('/').split('/').collect();
    if parts.len() != 3 || parts[1] != "evm" {
        return Err(bad("path must be /<project>/evm/<chainId>"));
    }
    let authority = match authority.contains(':') {
        true => authority.to_string(),
        false => format!("{authority}:{}", if tls { 443 } else { 80 }),
    };
    let http = if tls { "https" } else { "http" };
    Ok(Target { uri: format!("{http}://{authority}"), tls, project: parts[0].into(), chain: parts[2].into() })
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Result<Vec<u8>, String> {
    let s = s.trim_start_matches("0x");
    (0..s.len())
        .step_by(2)
        .map(|i| s.get(i..i + 2).and_then(|d| u8::from_str_radix(d, 16).ok()).ok_or(format!("bad hex {s}")))
        .collect()
}

fn token(number: u64, hash: &[u8]) -> String {
    format!("0x{number:x}:0x{}", hex(hash))
}

/// The app: its logs and the cursor of the last block it applied.
struct App {
    cursor: Option<(u64, Bytes)>,
    logs: HashMap<(Bytes, u32), (u64, Bytes)>,
    blocks: usize,
    limit: usize,
}

impl App {
    fn load(limit: usize) -> Result<App, Box<dyn Error>> {
        let cursor = match fs::read_to_string(CURSOR_PATH) {
            Ok(s) => {
                let (num, hash) = s.trim().split_once(':').ok_or("bad cursor")?;
                Some((u64::from_str_radix(num.trim_start_matches("0x"), 16)?, Bytes::from(unhex(hash)?)))
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => None,
            Err(e) => return Err(e.into()),
        };
        Ok(App { cursor, logs: HashMap::new(), blocks: 0, limit })
    }

    /// Writes a temporary file, syncs it and renames it over the cursor, so a
    /// crash leaves the old token or the new one.
    fn save(&mut self, number: u64, hash: Bytes) -> io::Result<()> {
        let tmp = format!("{CURSOR_PATH}.tmp");
        let f = fs::File::create(&tmp)?;
        io::Write::write_all(&mut &f, format!("{}\n", token(number, &hash)).as_bytes())?;
        f.sync_all()?;
        fs::rename(&tmp, CURSOR_PATH)?;
        self.cursor = Some((number, hash));
        Ok(())
    }

    /// Idempotent: a block applied again (a crash before the cursor save)
    /// writes the same keys.
    fn apply(&mut self, block: &BlockRef, logs: Vec<Log>) -> io::Result<()> {
        println!("block {} 0x{}: {} logs", block.number, hex(&block.hash), logs.len());
        for log in logs {
            self.logs.insert((log.block_hash, log.log_index), (block.number, log.data));
        }
        self.blocks += 1;
        self.save(block.number, block.hash.clone())
    }

    fn undo(&mut self, fork_point: &BlockRef) -> io::Result<()> {
        println!("undo to {} 0x{}", fork_point.number, hex(&fork_point.hash));
        self.logs.retain(|_, (n, _)| *n <= fork_point.number);
        self.save(fork_point.number, fork_point.hash.clone())
    }

    fn done(&self) -> bool {
        self.limit > 0 && self.blocks >= self.limit
    }
}

/// How a session ended.
enum End {
    Done,
    Restart,
    Watchdog,
    Closed,
    Failed(Status),
}

/// One stream from the cursor until it ends; true when a message arrived.
async fn session(app: &mut App, t: &Target, filter: &LogFilter, secret: Option<&str>) -> Result<(bool, End), Box<dyn Error>> {
    // The server's windows: with the 64 KiB defaults, log bursts wait on
    // this client's acknowledgements.
    let mut ep = Endpoint::from_shared(t.uri.clone())?
        .initial_stream_window_size(4 << 20)
        .initial_connection_window_size(16 << 20);
    if t.tls {
        ep = ep.tls_config(ClientTlsConfig::new().with_native_roots())?;
    }
    // A fresh channel per session: a black-holed connection is never reused.
    let channel = match ep.connect().await {
        Ok(c) => c,
        Err(e) => return Ok((false, End::Failed(Status::unavailable(e.to_string())))),
    };
    let mut req = Request::new(StreamLogsRequest {
        from_block: app.cursor.as_ref().map(|c| c.0),
        from_hash: app.cursor.as_ref().map(|c| c.1.clone()),
        filter: Some(filter.clone()),
        finality: Finality::Latest as i32,
        reorg: Reorg::Undo as i32,
        heartbeat_seconds: Some(HEARTBEAT),
        ..Default::default()
    });
    req.metadata_mut().insert("x-erpc-project", t.project.parse()?);
    req.metadata_mut().insert("x-erpc-chain-id", t.chain.parse()?);
    if let Some(s) = secret {
        req.metadata_mut().insert("x-erpc-secret-token", s.parse()?);
    }
    let mut stream = match StreamServiceClient::new(channel).stream_logs(req).await {
        Ok(r) => r.into_inner(),
        Err(s) => return Ok((false, End::Failed(s))),
    };
    let watchdog = Duration::from_secs(3 * HEARTBEAT as u64);
    let mut flowed = false;
    let mut part: Option<(BlockRef, Vec<Log>)> = None; // the block in progress
    loop {
        let msg = match tokio::time::timeout(watchdog, stream.message()).await {
            Err(_) => return Ok((flowed, End::Watchdog)),
            Ok(Err(s)) => return Ok((flowed, End::Failed(s))),
            Ok(Ok(None)) => return Ok((flowed, End::Closed)),
            Ok(Ok(Some(m))) => m,
        };
        flowed = true;
        match msg.message {
            Some(Message::Block(bl)) => {
                let Some(r) = bl.block else { continue };
                if part.as_ref().is_none_or(|(p, _)| p.hash != r.hash) {
                    part = Some((r.clone(), Vec::new()));
                }
                if let Some((_, logs)) = part.as_mut() {
                    logs.extend(bl.logs);
                }
                if !bl.complete {
                    continue;
                }
                let Some((r, logs)) = part.take() else { continue };
                match &app.cursor {
                    Some((_, h)) if *h == r.hash => continue, // already applied: skip
                    Some((_, h)) if *h != r.parent_hash => return Ok((flowed, End::Restart)),
                    _ => app.apply(&r, logs)?,
                }
            }
            Some(Message::Undo(u)) => {
                part = None;
                if let Some(fp) = u.fork_point {
                    app.undo(&fp)?;
                }
            }
            Some(Message::Status(st)) => {
                let state = State::try_from(st.state).map_or("unknown", |s| s.as_str_name());
                println!("status {state} {}", st.reason);
            }
            Some(Message::Heartbeat(_)) | None => {}
        }
        if app.done() {
            return Ok((flowed, End::Done));
        }
    }
}

fn out_of_range(s: &Status, cursor: &Option<(u64, Bytes)>) -> String {
    let from = cursor.as_ref().map_or("none".into(), |(n, h)| token(*n, h));
    match s.get_details_error_info() {
        Some(info) => {
            let n: u64 = info.metadata.get("oldestBlock").and_then(|v| v.parse().ok()).unwrap_or(0);
            let h = info.metadata.get("oldestHash").cloned().unwrap_or_default();
            format!("cursor {from} out of range ({}); oldest resumable 0x{n:x}:{h}", info.reason)
        }
        None => format!("cursor {from} out of range: {}", s.message()),
    }
}

async fn run() -> Result<(), Box<dyn Error>> {
    let t = parse_endpoint(&env::var("ERPC_ENDPOINT")?)?;
    let mut filter = LogFilter::default();
    if let Ok(a) = env::var("ERPC_ADDRESS") {
        filter.address.push(Bytes::from(unhex(&a)?));
    }
    let secret = env::var("ERPC_TOKEN").ok();
    let mut app = App::load(env::var("BLOCKS").map_or(0, |b| b.parse().unwrap_or(0)))?;
    let mut attempt = 0;
    loop {
        let began = Instant::now();
        let (flowed, end) = session(&mut app, &t, &filter, secret.as_deref()).await?;
        if began.elapsed() >= Duration::from_secs(60) {
            attempt = 0;
        }
        let at_once = match end {
            End::Done => return Ok(()),
            End::Restart => true, // linkage broke: resume from the cursor
            End::Watchdog | End::Closed => flowed,
            End::Failed(s) => match s.code() {
                Code::OutOfRange => return Err(out_of_range(&s, &app.cursor).into()),
                Code::Aborted | Code::Unauthenticated | Code::PermissionDenied | Code::InvalidArgument => {
                    return Err(s.into())
                }
                // Drained, aged out, stalled or lagging: the cursor loses nothing.
                c => flowed && c == Code::Unavailable,
            },
        };
        if at_once {
            continue;
        }
        let delay = (0.5 * 2f64.powi(attempt.min(6))).min(30.0);
        attempt += 1;
        println!("reconnect in up to {delay}s");
        tokio::time::sleep(Duration::from_secs_f64(delay / 2.0 + rand::rng().random_range(0.0..=delay / 2.0))).await;
    }
}

#[tokio::main]
async fn main() {
    if let Err(e) = run().await {
        // Out of range: away longer than the server's window. Backfill the
        // blocks after the cursor through the oldest resumable block with
        // eth_getLogs, save that block's token to erpc.cursor, then start again.
        eprintln!("{e}");
        std::process::exit(1);
    }
}
```

### Java

```xml filename="docs/examples/streams/java/pom.xml"
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 https://maven.apache.org/xsd/maven-4.0.0.xsd">
  <modelVersion>4.0.0</modelVersion>
  <groupId>example</groupId>
  <artifactId>stream-logs</artifactId>
  <version>0.1.0</version>
  <properties>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
    <maven.compiler.release>17</maven.compiler.release>
    <grpc.version>1.85.1</grpc.version>
  </properties>
  <dependencies>
    <dependency>
      <groupId>io.github.blockchain-data-standards</groupId>
      <artifactId>bds</artifactId>
      <version>0.1.0</version>
    </dependency>
    <dependency>
      <groupId>io.grpc</groupId>
      <artifactId>grpc-netty-shaded</artifactId>
      <version>${grpc.version}</version>
    </dependency>
  </dependencies>
  <build>
    <plugins>
      <plugin>
        <groupId>org.apache.maven.plugins</groupId>
        <artifactId>maven-compiler-plugin</artifactId>
        <version>3.14.1</version>
      </plugin>
      <plugin>
        <groupId>org.codehaus.mojo</groupId>
        <artifactId>exec-maven-plugin</artifactId>
        <version>3.6.4</version>
        <configuration>
          <mainClass>example.StreamLogs</mainClass>
        </configuration>
      </plugin>
    </plugins>
  </build>
</project>
```

```java filename="docs/examples/streams/java/src/main/java/example/StreamLogs.java"
// Logs from eRPC's StreamLogs on the bds stubs, applied to a store keyed by
// (blockHash, logIndex) and resumed from erpc.cursor, with the toolkit rules
// done by hand: window, heartbeat watchdog, reconnect, undo, out of range.
// Exits after BLOCKS blocks (default: never).
//
//   ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… mvn -q compile exec:java
package example;

import bds.evm.Models.BlockRef;
import bds.evm.Models.Log;
import bds.evm.Query.LogFilter;
import bds.evm.Stream.Finality;
import bds.evm.Stream.Reorg;
import bds.evm.Stream.StreamLogsRequest;
import bds.evm.Stream.StreamLogsResponse;
import bds.evm.StreamServiceGrpc;
import com.google.protobuf.Any;
import com.google.protobuf.ByteString;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.rpc.ErrorInfo;
import io.grpc.Context;
import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.protobuf.StatusProto;
import io.grpc.stub.MetadataUtils;
import java.io.IOException;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.NoSuchFileException;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.nio.file.StandardOpenOption;
import java.util.ArrayList;
import java.util.EnumSet;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.Iterator;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ThreadLocalRandom;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;

public final class StreamLogs {
  /** Seconds; the watchdog reconnects after 3 silent ones. */
  static final int HEARTBEAT = 15;
  static final Path CURSOR_PATH = Path.of("erpc.cursor");
  static final EnumSet<Status.Code> TERMINAL = EnumSet.of(Status.Code.OUT_OF_RANGE, Status.Code.ABORTED,
      Status.Code.UNAUTHENTICATED, Status.Code.PERMISSION_DENIED, Status.Code.INVALID_ARGUMENT);
  static final HexFormat HEX = HexFormat.of();

  /** A parsed grpc(s)://host:port/<project>/evm/<chainId>. */
  record Target(String host, int port, boolean tls, String project, String chain) {
    static Target parse(String raw) {
      URI u = URI.create(raw);
      boolean tls = switch (u.getScheme()) {
        case "grpc" -> false;
        case "grpcs" -> true;
        default -> throw new IllegalArgumentException(raw + ": scheme must be grpc or grpcs");
      };
      String[] parts = u.getPath().replaceAll("^/|/$", "").split("/");
      if (parts.length != 3 || !parts[1].equals("evm")) {
        throw new IllegalArgumentException(raw + ": path must be /<project>/evm/<chainId>");
      }
      int port = u.getPort() > 0 ? u.getPort() : tls ? 443 : 80;
      return new Target(u.getHost(), port, tls, parts[0], parts[2]);
    }
  }

  /** How a session ended: code is null when done, "restart", "watchdog" or a gRPC code name. */
  record End(boolean flowed, String code, StatusRuntimeException error) {}

  static String token(long number, ByteString hash) {
    return "0x" + Long.toHexString(number) + ":0x" + HEX.formatHex(hash.toByteArray());
  }

  /** The app: its logs and the cursor of the last block it applied. */
  static final class App {
    record Key(ByteString blockHash, int logIndex) {}

    record Held(long block, ByteString data) {}

    final Map<Key, Held> logs = new HashMap<>();
    final int limit;
    Long cursorNumber;
    ByteString cursorHash;
    int blocks;

    App(int limit) throws IOException {
      this.limit = limit;
      try {
        String[] t = Files.readString(CURSOR_PATH).trim().split(":");
        cursorNumber = Long.parseUnsignedLong(t[0].substring(2), 16);
        cursorHash = ByteString.copyFrom(HEX.parseHex(t[1].substring(2)));
      } catch (NoSuchFileException e) {
        // no cursor: the stream starts at the server's bound
      }
    }

    /** Writes a temporary file, syncs it and renames it over the cursor. */
    void save(long number, ByteString hash) throws IOException {
      Path tmp = Path.of(CURSOR_PATH + ".tmp");
      Files.writeString(tmp, token(number, hash) + "\n", StandardCharsets.UTF_8, StandardOpenOption.CREATE,
          StandardOpenOption.TRUNCATE_EXISTING, StandardOpenOption.WRITE, StandardOpenOption.SYNC);
      Files.move(tmp, CURSOR_PATH, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
      cursorNumber = number;
      cursorHash = hash;
    }

    /** Idempotent: a block applied again (a crash before the cursor save) writes the same keys. */
    void apply(BlockRef block, List<Log> blockLogs) throws IOException {
      for (Log l : blockLogs) {
        logs.put(new Key(l.getBlockHash(), l.getLogIndex()), new Held(block.getNumber(), l.getData()));
      }
      save(block.getNumber(), block.getHash());
      blocks++;
      System.out.printf("block %d 0x%s: %d logs%n", block.getNumber(), HEX.formatHex(block.getHash().toByteArray()),
          blockLogs.size());
    }

    void undo(BlockRef forkPoint) throws IOException {
      logs.values().removeIf(h -> h.block() > forkPoint.getNumber());
      save(forkPoint.getNumber(), forkPoint.getHash());
      System.out.printf("undo to %d 0x%s%n", forkPoint.getNumber(), HEX.formatHex(forkPoint.getHash().toByteArray()));
    }

    boolean done() {
      return limit > 0 && blocks >= limit;
    }
  }

  /** One stream from the cursor until it ends. */
  static End session(App app, Target t, LogFilter filter, String secret) throws IOException {
    // The server's 4 MiB window (grpc-java sets one for streams and the
    // connection): with the 64 KiB default, log bursts wait on this client.
    NettyChannelBuilder b = NettyChannelBuilder.forAddress(t.host(), t.port()).flowControlWindow(4 << 20);
    // A fresh channel per session: a black-holed connection is never reused.
    ManagedChannel channel = (t.tls() ? b.useTransportSecurity() : b.usePlaintext()).build();
    Metadata md = new Metadata();
    md.put(Metadata.Key.of("x-erpc-project", Metadata.ASCII_STRING_MARSHALLER), t.project());
    md.put(Metadata.Key.of("x-erpc-chain-id", Metadata.ASCII_STRING_MARSHALLER), t.chain());
    if (secret != null) {
      md.put(Metadata.Key.of("x-erpc-secret-token", Metadata.ASCII_STRING_MARSHALLER), secret);
    }
    StreamLogsRequest.Builder req = StreamLogsRequest.newBuilder().setFilter(filter)
        .setFinality(Finality.FINALITY_LATEST).setReorg(Reorg.REORG_UNDO).setHeartbeatSeconds(HEARTBEAT);
    if (app.cursorNumber != null) {
      req.setFromBlock(app.cursorNumber).setFromHash(app.cursorHash);
    }
    Context.CancellableContext ctx = Context.current().withCancellation();
    ScheduledExecutorService timer = Executors.newSingleThreadScheduledExecutor();
    AtomicLong last = new AtomicLong(System.nanoTime());
    AtomicBoolean fired = new AtomicBoolean();
    long limit = TimeUnit.SECONDS.toNanos(3L * HEARTBEAT);
    timer.scheduleAtFixedRate(() -> {
      if (System.nanoTime() - last.get() > limit && fired.compareAndSet(false, true)) {
        ctx.cancel(null);
      }
    }, limit / 12, limit / 12, TimeUnit.NANOSECONDS);
    Context previous = ctx.attach();
    boolean flowed = false;
    BlockRef part = null; // the block in progress and its logs so far
    List<Log> partLogs = new ArrayList<>();
    try {
      Iterator<StreamLogsResponse> it = StreamServiceGrpc.newBlockingStub(channel)
          .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(md)).streamLogs(req.build());
      while (it.hasNext()) {
        StreamLogsResponse msg = it.next();
        flowed = true;
        last.set(System.nanoTime());
        switch (msg.getMessageCase()) {
          case BLOCK -> {
            BlockRef ref = msg.getBlock().getBlock();
            if (part == null || !part.getHash().equals(ref.getHash())) {
              part = ref;
              partLogs = new ArrayList<>();
            }
            partLogs.addAll(msg.getBlock().getLogsList());
            if (!msg.getBlock().getComplete()) {
              continue;
            }
            part = null;
            if (ref.getHash().equals(app.cursorHash)) {
              continue; // already applied: skip
            }
            if (app.cursorHash != null && !ref.getParentHash().equals(app.cursorHash)) {
              return new End(flowed, "restart", null); // linkage broke: resume from the cursor
            }
            app.apply(ref, partLogs);
          }
          case UNDO -> {
            part = null;
            app.undo(msg.getUndo().getForkPoint());
          }
          case STATUS -> System.out.printf("status %s %s%n", msg.getStatus().getState(), msg.getStatus().getReason());
          default -> {}
        }
        last.set(System.nanoTime());
        if (app.done()) {
          return new End(flowed, null, null);
        }
      }
      return new End(flowed, "UNAVAILABLE", null); // the server closed the stream
    } catch (StatusRuntimeException e) {
      if (fired.get()) {
        System.out.println("watchdog: no message within 3 heartbeats");
        return new End(flowed, "watchdog", null);
      }
      return new End(flowed, e.getStatus().getCode().name(), e);
    } finally {
      ctx.detach(previous);
      ctx.cancel(null);
      timer.shutdownNow();
      channel.shutdownNow();
    }
  }

  /** The out-of-range end as a message: the cursor and the oldest block a stream can resume from. */
  static String outOfRange(StatusRuntimeException e, App app) {
    String from = app.cursorHash == null ? "none" : token(app.cursorNumber, app.cursorHash);
    com.google.rpc.Status st = StatusProto.fromThrowable(e);
    for (Any d : st == null ? List.<Any>of() : st.getDetailsList()) {
      if (!d.is(ErrorInfo.class)) {
        continue;
      }
      try {
        ErrorInfo info = d.unpack(ErrorInfo.class);
        long n = Long.parseUnsignedLong(info.getMetadataOrDefault("oldestBlock", "0"));
        return String.format("cursor %s out of range (%s); oldest resumable 0x%x:%s", from,
            info.getReason(), n, info.getMetadataOrDefault("oldestHash", ""));
      } catch (InvalidProtocolBufferException ignored) {
        // not an ErrorInfo after all; fall through to the status message
      }
    }
    return "cursor " + from + " out of range: " + e.getStatus().getDescription();
  }

  public static void main(String[] args) throws Exception {
    Target t = Target.parse(System.getenv("ERPC_ENDPOINT"));
    LogFilter.Builder filter = LogFilter.newBuilder();
    String address = System.getenv("ERPC_ADDRESS");
    if (address != null) {
      filter.addAddress(ByteString.copyFrom(HEX.parseHex(address.substring(2))));
    }
    String blocks = System.getenv("BLOCKS");
    App app = new App(blocks == null ? 0 : Integer.parseInt(blocks));
    int attempt = 0;
    while (true) {
      long began = System.nanoTime();
      End end = session(app, t, filter.build(), System.getenv("ERPC_TOKEN"));
      if (end.code() == null) {
        return;
      }
      if (end.error() != null && TERMINAL.contains(end.error().getStatus().getCode())) {
        if (end.error().getStatus().getCode() == Status.Code.OUT_OF_RANGE) {
          // Away longer than the server's window. Backfill the blocks after
          // the cursor through the oldest resumable block with eth_getLogs,
          // save that block's token to erpc.cursor, then start again.
          System.err.println(outOfRange(end.error(), app));
          System.exit(1);
        }
        throw end.error();
      }
      if (System.nanoTime() - began >= TimeUnit.SECONDS.toNanos(60)) {
        attempt = 0;
      }
      boolean atOnce = end.code().equals("restart")
          || end.flowed() && (end.code().equals("watchdog") || end.code().equals("UNAVAILABLE"));
      if (atOnce) {
        continue; // drained, aged out, stalled or silent: the cursor loses nothing
      }
      double delay = Math.min(30.0, 0.5 * Math.pow(2, Math.min(attempt, 6)));
      attempt++;
      System.out.printf("reconnect after %s%n", end.code());
      Thread.sleep((long) (1000 * (delay / 2 + ThreadLocalRandom.current().nextDouble(delay / 2))));
    }
  }
}
```

## 4. ws extended

For clients that stay on WebSocket: add `{"erpc": {...}}` as the last `eth_subscribe` param. Data frames stay standard `eth_subscription`; control frames (`blockEnd`, `undo`, `heartbeat`, `status`, `end`) use the method `erpc_subscription`. The options, frames and end reasons are in [Extended subscriptions](https://docs.erpc.cloud/operation/websocket.llms.txt#extended-subscriptions). For a backend:

- `reorg: "undo"`, `blockEnd: true` (logs), `heartbeat`, and `from` = the cursor on every resubscribe.
- Hold a block's logs until its `blockEnd`; apply them then, and save `<number>:<hash>` of the `blockEnd` block. `newHeads`: the cursor is the last head.
- `end` removes the subscription and keeps the connection: `out_of_range` and `final_reverted` are terminal; `lagging` resubscribes; `stalled` reconnects (the instance lags); `draining` is followed by a close `1001`.
- The same watchdog and reconnect rules as section 3.

> **WARNING**
> Options in the ws URL apply to every subscription on the connection. With `reorg=undo` there, standard libraries (viem, ethers, geth) miss reorgs: they ignore `erpc_subscription` frames.

```js filename="docs/examples/streams/ws/ws_extended.mjs"
// Logs over an extended ws subscription, applied per block at its blockEnd
// to a store keyed by (blockHash, logIndex) and resumed from erpc.cursor:
// from, reorg undo, blockEnd, heartbeat watchdog and reconnect by hand.
// Node ≥ 22 (global WebSocket), no dependencies. Exits after BLOCKS blocks
// (default: never).
//
//   ERPC_WS=wss://erpc.example/main/evm/1 ERPC_ADDRESS=0x… node ws_extended.mjs
import { existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";

const HEARTBEAT = 15; // seconds; the watchdog reconnects after 3 silent ones
const CURSOR_PATH = "erpc.cursor";
const TERMINAL = new Set(["out_of_range", "final_reverted"]);
const limit = Number(process.env.BLOCKS ?? 0);

let cursor = existsSync(CURSOR_PATH) ? readFileSync(CURSOR_PATH, "utf8").trim() : undefined; // <numberHex>:<hash>
const logs = new Map(); // `${blockHash}:${logIndex}` → { block, data }
let blocks = 0;
let attempt = 0;

function save(number, hash) {
  cursor = `${number}:${hash}`;
  writeFileSync(`${CURSOR_PATH}.tmp`, `${cursor}\n`);
  renameSync(`${CURSOR_PATH}.tmp`, CURSOR_PATH);
}

function connect() {
  const ws = new WebSocket(process.env.ERPC_WS);
  const opened = Date.now();
  let subscription;
  let flowed = false;
  let last = Date.now();
  let part = new Map(); // block hash → logs received before its blockEnd
  const watchdog = setInterval(() => {
    if (Date.now() - last > 3 * HEARTBEAT * 1000) {
      console.log("watchdog: no message within 3 heartbeats");
      ws.close();
    }
  }, 1000);

  const subscribe = () => {
    part = new Map();
    const filter = process.env.ERPC_ADDRESS ? { address: process.env.ERPC_ADDRESS } : {};
    const erpc = { reorg: "undo", blockEnd: true, heartbeat: HEARTBEAT, ...(cursor && { from: cursor }) };
    ws.send(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "eth_subscribe", params: ["logs", filter, { erpc }] }));
  };

  ws.onopen = subscribe;
  ws.onmessage = (ev) => {
    last = Date.now();
    flowed = true;
    const msg = JSON.parse(ev.data);
    if (msg.id === 1) {
      if (msg.error) throw new Error(`eth_subscribe: ${msg.error.code} ${msg.error.message}`);
      subscription = msg.result;
      return;
    }
    if (msg.params?.subscription !== subscription) return;
    const r = msg.params.result;
    if (msg.method === "eth_subscription") {
      // A log: held until its block's blockEnd.
      if (!part.has(r.blockHash)) part.set(r.blockHash, []);
      part.get(r.blockHash).push(r);
      return;
    }
    if (msg.method !== "erpc_subscription") return;
    switch (r.type) {
      case "blockEnd": {
        const blockLogs = part.get(r.block.hash) ?? [];
        part.delete(r.block.hash);
        if (cursor?.endsWith(`:${r.block.hash}`)) return; // already applied: skip
        if ((cursor && !cursor.endsWith(`:${r.block.parentHash}`)) || blockLogs.length !== r.logs) {
          ws.close(); // linkage broke or a log is missing: resume from the cursor
          return;
        }
        // Idempotent: a block applied again (a crash before the save) writes the same keys.
        for (const l of blockLogs) logs.set(`${l.blockHash}:${BigInt(l.logIndex)}`, { block: BigInt(r.block.number), data: l.data });
        save(r.block.number, r.block.hash);
        console.log(`block ${BigInt(r.block.number)} ${r.block.hash}: ${blockLogs.length} logs`);
        if (++blocks === limit) {
          clearInterval(watchdog);
          ws.onclose = null;
          ws.close();
        }
        return;
      }
      case "undo": {
        part = new Map();
        const fork = BigInt(r.forkPoint.number);
        for (const [key, l] of logs) if (l.block > fork) logs.delete(key);
        save(r.forkPoint.number, r.forkPoint.hash);
        console.log(`undo to ${fork} ${r.forkPoint.hash}`);
        return;
      }
      case "status":
        console.log(`status ${r.state} ${r.reason ?? ""}`);
        return;
      case "heartbeat":
        return;
      case "end":
        if (TERMINAL.has(r.reason)) {
          // out_of_range: away longer than the server's window. Backfill the
          // blocks after the cursor through r.oldest with eth_getLogs, save
          // r.oldest's token to erpc.cursor, then start again.
          console.error(`end ${r.reason} ${r.detail ?? ""}: cursor ${cursor}, oldest ${r.oldest ? `${r.oldest.number}:${r.oldest.hash}` : "unknown"}`);
          process.exit(1);
        }
        // The subscription is gone, the connection stays. stalled: this
        // instance lags, so reconnect (a load balancer may pick another);
        // lagging: resubscribe here. draining: the close (1001) follows.
        if (r.reason === "stalled") ws.close();
        else if (r.reason !== "draining") subscribe();
    }
  };
  ws.onclose = (ev) => {
    clearInterval(watchdog);
    if (Date.now() - opened >= 60_000) attempt = 0;
    // Drained (1001) or silent after messages flowed: the cursor loses nothing.
    if (flowed) return connect();
    const delay = Math.min(30, 0.5 * 2 ** Math.min(attempt++, 6));
    console.log(`reconnect after close ${ev.code}`);
    setTimeout(connect, 1000 * (delay / 2 + (Math.random() * delay) / 2));
  };
}

connect();
```

## 5. Check list

1. Server: `server.grpcEnabled: true` (gRPC) or `server.webSocket.enabled: true` (ws).
2. Endpoint `grpc(s)://host:port/<project>/evm/<chainId>`; auth in `x-erpc-secret-token`.
3. Finality chosen; `undo` implemented unless `finalized`.
4. Every write keyed by the dedup key; applying a block twice changes nothing.
5. Cursor stored durably, ideally in the app's database; saved only after a block is applied.
6. Out of range handled: backfill from the cursor through `oldest`, save `oldest`, restart.
7. `FinalRevertedError` / `ABORTED` / `final_reverted` handled for `finalized` streams.
8. By hand only: windows, `heartbeatSeconds`, watchdog, reconnect policy, terminal codes.
9. The app runs against a local eRPC and survives a restart of eRPC and of itself with no gap.
