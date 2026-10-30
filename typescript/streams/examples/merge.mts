// New heads from two regions merged into one chain: either region may go
// away and the app keeps its blocks, undoing only on a real branch switch.
import { stream } from "@erpc-cloud/streams";

const s = stream({
  endpoints: [
    "grpcs://erpc-a.example:443/main/evm/8453",
    "grpcs://erpc-b.example:443/main/evm/8453",
  ],
  kind: "newHeads",
  finality: "depth:3",
});

await s.run({
  apply: (block) => console.log(`${block.number} ${block.hash}`),
  undo: (forkPoint) => console.log(`undo to ${forkPoint.number}`),
});
