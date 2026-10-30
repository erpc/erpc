# @erpc-cloud/streams

Gap-free, reorg-aware streams of new heads, logs and pending transactions from eRPC's gRPC `StreamService`, for Node ≥ 22. Same API as the Go library `github.com/erpc/erpc/sdk/go/streams`.

```sh
npm install @erpc-cloud/streams
```

```ts
import { fileCursor, stream } from "@erpc-cloud/streams";

const s = stream({
  endpoints: ["grpcs://erpc-a.example:443/main/evm/1"], // 1, or 2 to merge
  kind: "logs", // "newHeads" | "newPendingTransactions"
  filter: { address: ["0xa0b8…"], topics: [["0xddf2…"]] },
  finality: "latest", // "safe" | "finalized" | "depth:12"
  cursor: fileCursor("./erpc.cursor"),
  metadata: { "x-erpc-secret-token": process.env.ERPC_TOKEN! },
});
await s.run({
  apply: async (block) => { /* block.logs; idempotent by log.key */ },
  undo: async (forkPoint) => { /* drop state above forkPoint.number */ },
  status: (st) => {}, // ok | degraded | lagging | reconnecting
});
```

- The cursor is saved after `apply` or `undo` returns: delivery is at-least-once.
- `run` resolves when `options.signal` aborts; it rejects on a terminal end: `OutOfRangeError` (`cursor`, `oldest`), `FinalRevertedError`, the gRPC error for auth or invalid options, or an error your handler threw.
- Reconnects, the heartbeat watchdog, deduplication and the two-endpoint merge are the library's.

More: [examples/](examples/). Integration guide (cursor storage, reorgs, merge, out of range): [INTEGRATION.md](INTEGRATION.md), generated from the eRPC docs page [Streams integration](https://docs.erpc.cloud/operation/streams-integration).

## Development

```sh
pnpm install
pnpm generate   # src/gen from the manifesto protos eRPC's go.mod pins; needs go and protoc
pnpm typecheck
pnpm test       # builds cmd/erpc (or ERPC_BIN) and runs it over a simulated chain
```
