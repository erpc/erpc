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
