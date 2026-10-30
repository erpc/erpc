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
