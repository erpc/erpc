// The real-world suite's Node client: reads a config JSON on stdin, runs the
// library, and appends one JSON record per line for the oracle, each with
// "at" (unix ms):
//   {"t":"apply","number","hash","parentHash","keys"}  keys: log keys, [hash], or tx hashes
//   {"t":"undo","number","hash"}
//   {"t":"status","state","reason","endpoint"}
//   {"t":"end","error","code","cursor","oldest"}  last; error null on SIGTERM
// Config: StreamConfig fields, plus "cursorPath" for a file cursor and
// "recordsPath" for the records file (else stdout).
import { appendFileSync } from "node:fs";
import { text } from "node:stream/consumers";
import { status } from "@grpc/grpc-js";
import {
  FinalRevertedError,
  fileCursor,
  MessageTooLargeError,
  OutOfRangeError,
  stream,
} from "../lib/index.js";

const config = JSON.parse(await text(process.stdin));
const { cursorPath, recordsPath, ...rest } = config;
// Synchronous, so every record the app saw is on disk when the driver is killed.
const write = (rec) => {
  const line = `${JSON.stringify({ ...rec, at: Date.now() })}\n`;
  if (recordsPath) appendFileSync(recordsPath, line);
  else process.stdout.write(line);
};

const ac = new AbortController();
process.on("SIGTERM", () => ac.abort());
process.on("SIGINT", () => ac.abort());

const s = stream({ ...rest, cursor: cursorPath ? fileCursor(cursorPath) : undefined });
let end = { t: "end", error: null, code: null, cursor: null, oldest: null };
try {
  await s.run(
    {
      apply(b) {
        const keys =
          config.kind === "logs"
            ? b.logs.map((l) => l.key)
            : config.kind === "newHeads"
              ? [b.hash]
              : b.hashes.concat(b.transactions.map((t) => `0x${Buffer.from(t.hash).toString("hex")}`));
        write({ t: "apply", number: Number(b.number), hash: b.hash, parentHash: b.parentHash, keys });
      },
      undo(fp) {
        write({ t: "undo", number: Number(fp.number), hash: fp.hash });
      },
      status(st) {
        write({ t: "status", state: st.state, reason: st.reason, endpoint: st.endpoint });
      },
    },
    { signal: ac.signal },
  );
} catch (err) {
  end = {
    t: "end",
    error: err.message,
    code:
      err instanceof OutOfRangeError
        ? "OUT_OF_RANGE"
        : err instanceof FinalRevertedError
          ? "ABORTED"
          : err instanceof MessageTooLargeError
            ? "RESOURCE_EXHAUSTED"
            : typeof err.code === "number"
              ? status[err.code]
              : null,
    cursor: err.cursor ?? null,
    oldest: err.oldest ? { number: Number(err.oldest.number), hash: err.oldest.hash } : null,
  };
}
write(end);
process.exitCode = end.error === null ? 0 : 1;
