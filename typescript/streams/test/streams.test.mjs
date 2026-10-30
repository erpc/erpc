// The library against real eRPC pods over a simulated chain
// (test/streamsim): resume after a pod restart, undo on reorgs, the
// heartbeat watchdog on a black-holed link, a two-endpoint merge, the
// out-of-range end, stops on abort and on a handler error, a merge whose
// leader dies on another branch, and the message-too-large end.
// ERPC_BIN names a built cmd/erpc; else it is built.
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { after, before, test } from "node:test";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import {
  fileCursor,
  formatToken,
  MessageTooLargeError,
  OutOfRangeError,
  stream,
} from "../lib/index.js";

const root = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "..");
const dir = mkdtempSync(join(tmpdir(), "erpc-streams-"));
let sim;
let ready;
const replies = [];
const waiting = [];

/** Sends one streamsim command and waits for its reply. */
function send(cmd) {
  sim.stdin.write(`${JSON.stringify(cmd)}\n`);
  return new Promise((resolve, reject) => waiting.push({ resolve, reject }));
}

before(async () => {
  const bin = process.env.ERPC_BIN ?? join(dir, "erpc");
  if (!process.env.ERPC_BIN) {
    execFileSync("go", ["build", "-o", bin, "./cmd/erpc"], { cwd: root, stdio: "inherit" });
  }
  execFileSync("go", ["build", "-o", join(dir, "streamsim"), "./test/streamsim"], { cwd: root, stdio: "inherit" });
  sim = spawn(join(dir, "streamsim"), ["-erpc", bin, "-dir", dir, "-pods", "2"], {
    stdio: ["pipe", "pipe", "inherit"],
  });
  const lines = createInterface({ input: sim.stdout });
  const first = new Promise((resolve) => lines.once("line", (l) => resolve(JSON.parse(l))));
  ready = await first;
  lines.on("line", (l) => {
    const r = JSON.parse(l);
    const w = waiting.shift();
    if (r.error) w.reject(new Error(r.error));
    else w.resolve(r);
  });
  sim.on("exit", (code) => {
    for (const w of waiting.splice(0)) w.reject(new Error(`streamsim exited ${code}`));
  });
  // Pods serve once their trackers follow the chain.
  await send({ op: "wait", blocks: 12 });
});

after(() => sim?.stdin.end());

/**
 * An app that keeps the applied chain and its keys, and fails on what the
 * contract forbids: a gap, a broken parent link, a key applied twice on
 * one branch, an undo to a block it never applied.
 */
function app(kind) {
  const chain = [];
  const held = new Map();
  const statuses = [];
  const appliedAt = [];
  let undos = 0;
  const keysOf = (b) => (kind === "logs" ? b.logs.map((l) => l.key) : [b.hash]);
  return {
    chain,
    held,
    statuses,
    appliedAt,
    get undos() {
      return undos;
    },
    handler: {
      apply(b) {
        const last = chain.at(-1);
        if (last) {
          assert.equal(b.number, last.number + 1n, `block ${b.number} after ${last.number}`);
          assert.equal(b.parentHash, last.hash, `block ${b.number} does not link to ${last.number}`);
        }
        for (const k of keysOf(b)) {
          assert.ok(!held.has(k), `key ${k} applied twice on one branch`);
          held.set(k, b.number);
        }
        chain.push({ number: b.number, hash: b.hash });
        appliedAt.push(Date.now());
      },
      undo(fp) {
        undos++;
        const i = chain.findIndex((e) => e.number === fp.number);
        assert.ok(i >= 0 || fp.number < chain[0].number, `undo to ${fp.number}: never applied`);
        if (i >= 0) assert.equal(chain[i].hash, fp.hash, `undo to ${fp.number}: another branch`);
        chain.splice(i + 1);
        for (const [k, n] of held) if (n > fp.number) held.delete(k);
      },
      status(st) {
        statuses.push({ ...st, at: Date.now() });
      },
    },
  };
}

/**
 * The applied chain is the canonical one, or with view the one pod view's
 * vendors serve, with exactly its keys.
 */
async function requireCanonical(a, filter, view) {
  assert.ok(a.chain.length >= 20, `only ${a.chain.length} blocks applied`);
  const from = Number(a.chain[0].number);
  const to = Number(a.chain.at(-1).number);
  const { blocks } = await send({ op: "canon", from, to, filter, view });
  assert.equal(blocks.length, a.chain.length);
  const want = new Set();
  blocks.forEach((b, i) => {
    assert.equal(a.chain[i].hash, b.hash, `block ${b.number} not canonical`);
    for (const k of b.keys) want.add(k);
  });
  if (filter !== undefined) {
    assert.ok(want.size > 0, `filter ${filter} matched no log in ${from}-${to}`);
    assert.deepEqual(new Set(a.held.keys()), want);
  }
}

async function running(config, a) {
  const ac = new AbortController();
  const done = stream(config).run(a.handler, { signal: ac.signal });
  // A handler assertion fails the run; stop() rethrows it.
  done.catch(() => {});
  return {
    done,
    async stop() {
      ac.abort();
      await done;
    },
  };
}

test("logs resume after a pod restart and undo on reorgs", async () => {
  const a = app("logs");
  const r = await running(
    {
      endpoints: [ready.endpoints[0]],
      kind: "logs",
      filter: ready.filters[0],
      heartbeatSeconds: 2,
      cursor: fileCursor(join(dir, "logs.cursor")),
    },
    a,
  );
  await send({ op: "wait", blocks: 10 });
  await send({ op: "reorg", depth: 3 });
  await send({ op: "wait", blocks: 10 });
  await send({ op: "kill", pod: 0 });
  await send({ op: "wait", blocks: 8 });
  await send({ op: "start", pod: 0 });
  await send({ op: "wait", blocks: 20 });
  await send({ op: "reorg", depth: 2 });
  await send({ op: "wait", blocks: 10 });
  await r.stop();
  assert.ok(a.undos >= 2, `${a.undos} undos across 2 reorgs`);
  assert.ok(a.statuses.some((s) => s.state === "reconnecting"), "no reconnect across the restart");
  await requireCanonical(a, 0);
});

test("the watchdog reconnects a black-holed link within 3 heartbeats", async () => {
  const a = app("newHeads");
  const r = await running({ endpoints: [ready.endpoints[1]], kind: "newHeads", heartbeatSeconds: 1 }, a);
  await send({ op: "wait", blocks: 10 });
  const cutAt = Date.now();
  await send({ op: "cut", pod: 1, on: true });
  await send({ op: "wait", blocks: 24 });
  const fired = a.statuses.find((s) => s.at >= cutAt && s.state === "reconnecting");
  assert.equal(fired?.reason, "watchdog");
  assert.ok(fired.at - cutAt <= 3000 + 1000, `watchdog after ${fired.at - cutAt} ms`);
  await send({ op: "cut", pod: 1, on: false });
  await send({ op: "wait", blocks: 16 });
  await r.stop();
  await requireCanonical(a);
});

test("two endpoints merge through a pod kill, a reorg and a cut link", async () => {
  const a = app("logs");
  const r = await running(
    {
      endpoints: ready.endpoints,
      kind: "logs",
      filter: ready.filters[1],
      heartbeatSeconds: 1,
      cursor: fileCursor(join(dir, "merge.cursor")),
    },
    a,
  );
  await send({ op: "wait", blocks: 10 });
  await send({ op: "kill", pod: 0 });
  await send({ op: "reorg", depth: 2 });
  await send({ op: "wait", blocks: 10 });
  await send({ op: "start", pod: 0 });
  await send({ op: "wait", blocks: 10 });
  await send({ op: "cut", pod: 1, on: true });
  await send({ op: "reorg", depth: 3 });
  await send({ op: "wait", blocks: 10 });
  await send({ op: "cut", pod: 1, on: false });
  await send({ op: "wait", blocks: 10 });
  await r.stop();
  assert.ok(a.undos >= 2, `${a.undos} undos across 2 reorgs`);
  await requireCanonical(a, 1);
  // The other connection carried the app through each outage.
  const gap = Math.max(...a.appliedAt.slice(1).map((t, i) => t - a.appliedAt[i]));
  assert.ok(gap <= 3000, `${gap} ms without a block`);
});

test("a cursor below the window ends OutOfRangeError with the oldest block", async () => {
  const a = app("logs");
  const token = `0x1:0x${"ab".repeat(32)}`;
  await assert.rejects(
    stream({
      endpoints: [ready.endpoints[0]],
      kind: "logs",
      cursor: { load: () => token, save: () => assert.fail("saved a cursor") },
    }).run(a.handler),
    (err) => {
      assert.ok(err instanceof OutOfRangeError, String(err));
      assert.equal(err.cursor, token);
      assert.ok(err.oldest && err.oldest.number > 1n, `oldest ${err.oldest?.number}`);
      return true;
    },
  );
  assert.equal(a.chain.length, 0);
});

test("no apply or undo starts after the signal aborts, though the stream buffered more", async () => {
  const { tip } = await send({ op: "wait", blocks: 0 });
  const [back] = (await send({ op: "canon", from: tip.number - 12, to: tip.number - 12 })).blocks;
  const ac = new AbortController();
  const calls = [];
  await stream({
    endpoints: [ready.endpoints[0]],
    kind: "newHeads",
    heartbeatSeconds: 1,
    cursor: { load: () => formatToken({ number: BigInt(back.number), hash: back.hash }), save() {} },
  }).run(
    {
      async apply(b) {
        calls.push(`apply ${b.number}${ac.signal.aborted ? " after abort" : ""}`);
        if (calls.length === 3) ac.abort();
        // The server's burst fills the stream's buffer meanwhile.
        await sleep(100);
      },
      undo(fp) {
        calls.push(`undo ${fp.number}${ac.signal.aborted ? " after abort" : ""}`);
      },
    },
    { signal: ac.signal },
  );
  assert.equal(calls.length, 3, calls.join(", "));
});

test("a handler error stops both connections at once", async () => {
  const boom = new Error("app failed");
  const calls = [];
  await assert.rejects(
    stream({ endpoints: ready.endpoints, kind: "newHeads", heartbeatSeconds: 1 }).run({
      async apply(b) {
        calls.push(b.number);
        if (calls.length === 5) {
          // Both connections deliver the next blocks meanwhile.
          await sleep(600);
          throw boom;
        }
      },
      undo() {},
    }),
    (err) => err === boom,
  );
  assert.deepEqual(calls.slice(5), [], "apply after the handler failed");
});

// Pod 1 stays on the branch it serves while pod 0's chain reorgs below the
// split, so pod 0 leads; then pod 0 dies. The app must move to pod 1's
// branch: an undo to the fork point, then every block above it. Pod 0
// comes back and ends each stream out of range (cursor_unknown: the
// cursor is on a branch it never saw); the run must go on with pod 1.
test("two endpoints on different branches: a reorg, then the leader dies", async () => {
  const a = app("logs");
  const r = await running(
    { endpoints: ready.endpoints, kind: "logs", filter: ready.filters[1], heartbeatSeconds: 1 },
    a,
  );
  await send({ op: "wait", blocks: 10 });
  await send({ op: "pin", pod: 1, on: true });
  await send({ op: "reorg", depth: 3 });
  await send({ op: "wait", blocks: 6 });
  await send({ op: "kill", pod: 0 });
  await send({ op: "wait", blocks: 10 });
  await send({ op: "start", pod: 0 });
  await send({ op: "wait", blocks: 16 });
  const state = await Promise.race([r.done.then(() => "resolved", (e) => e), sleep(0).then(() => "running")]);
  assert.equal(state, "running", "the run ended while pod 1 streams");
  await r.stop();
  assert.ok(a.undos >= 1, "no undo across the branch switch");
  await requireCanonical(a, 1, 1);
});

// A pod whose send limit is below one log's message ends each logs stream
// RESOURCE_EXHAUSTED MESSAGE_TOO_LARGE naming the block; a resume meets
// the same block, so the run must stop with MessageTooLargeError, never
// reconnect.
test("a log over the server's message bound ends MessageTooLargeError", async () => {
  await send({ op: "kill", pod: 0 });
  await send({ op: "start", pod: 0, maxSendBytes: 160 });
  try {
    const a = app("logs");
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), 30_000);
    let err;
    try {
      await stream({
        endpoints: [ready.endpoints[0]],
        kind: "logs",
        filter: ready.filters[0],
        heartbeatSeconds: 1,
      }).run(a.handler, { signal: ac.signal });
    } catch (e) {
      err = e;
    } finally {
      clearTimeout(timer);
    }
    assert.ok(err instanceof MessageTooLargeError, `run ended ${err ?? "by the 30s abort"}`);
    assert.equal(err.maxBytes, 160);
    assert.ok(err.messageBytes > 160, `${err.messageBytes} bytes`);
    const n = Number(err.block.number);
    const { blocks } = await send({ op: "canon", from: n, to: n });
    assert.equal(err.block.hash, blocks[0].hash);
    assert.ok(
      a.chain.every((b) => b.number < err.block.number),
      `applied ${a.chain.at(-1)?.number}, at or above ${n}`,
    );
  } finally {
    await send({ op: "kill", pod: 0 });
    await send({ op: "start", pod: 0 });
  }
});
