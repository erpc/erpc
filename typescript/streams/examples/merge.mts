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
