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
