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
