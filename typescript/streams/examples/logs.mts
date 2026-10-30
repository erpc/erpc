// USDC transfers on Ethereum, applied once per branch, resumed from a file
// cursor after every restart.
import { fileCursor, OutOfRangeError, stream } from "@erpc-cloud/streams";

const transfers = new Map<string, { block: bigint; data: Buffer }>();

const s = stream({
  endpoints: [process.env.ERPC_ENDPOINT ?? "grpcs://erpc-a.example:443/main/evm/1"],
  kind: "logs",
  filter: {
    address: ["0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"],
    topics: [["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]],
  },
  finality: "latest",
  cursor: fileCursor("./erpc.cursor"),
  metadata: { "x-erpc-secret-token": process.env.ERPC_TOKEN ?? "" },
});

const ac = new AbortController();
process.on("SIGINT", () => ac.abort());

try {
  await s.run(
    {
      apply: (block) => {
        for (const log of block.logs) {
          transfers.set(log.key, { block: block.number, data: log.data });
        }
      },
      undo: (forkPoint) => {
        for (const [key, t] of transfers) {
          if (t.block > forkPoint.number) transfers.delete(key);
        }
      },
      status: (st) => console.error(`${st.endpoint}: ${st.state} ${st.reason}`),
    },
    { signal: ac.signal },
  );
} catch (err) {
  if (err instanceof OutOfRangeError) {
    // Away longer than the window: backfill from cursor to err.oldest
    // yourself, then save a cursor at or above err.oldest.
    console.error(err.message);
    process.exit(2);
  }
  throw err;
}
