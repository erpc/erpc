#!/usr/bin/env python3
import argparse
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def _hex_to_int(s: str) -> int:
    if s is None:
        return 0
    s = str(s).strip().lower()
    if s.startswith("0x"):
        return int(s, 16)
    return int(s)


def _write_json(w, obj) -> None:
    data = json.dumps(obj, separators=(",", ":"), ensure_ascii=True).encode("utf-8")
    w.send_response(200)
    w.send_header("content-type", "application/json")
    w.send_header("content-length", str(len(data)))
    w.end_headers()
    w.wfile.write(data)


class Handler(BaseHTTPRequestHandler):
    server_version = "mock-evm-upstream/1.0"

    def log_message(self, fmt, *args):
        # Silence default access logs; caller can add tcpdump/mitm if needed.
        return

    def do_POST(self):
        try:
            length = int(self.headers.get("content-length") or "0")
        except Exception:
            length = 0
        body = self.rfile.read(length) if length > 0 else b""
        try:
            req = json.loads(body.decode("utf-8") or "{}")
        except Exception:
            self.send_response(400)
            self.end_headers()
            return

        req_id = req.get("id", 1)
        method = req.get("method", "")
        params = req.get("params") or []

        def reply(result):
            _write_json(self, {"jsonrpc": "2.0", "id": req_id, "result": result})

        if method == "eth_chainId":
            reply(self.server.chain_id_hex)
            return
        if method == "eth_syncing":
            reply(False)
            return
        if method == "eth_blockNumber":
            reply(self.server.latest_block_hex)
            return
        if method == "eth_getBlockByNumber":
            reply({"number": self.server.latest_block_hex, "timestamp": "0x1"})
            return

        if method == "eth_getBlockReceipts":
            # Minimal receipts array; includes a Transfer topic so getLogs fallback filter can match.
            # (Content not consensus-accurate; only used for local stress runs.)
            transfer = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
            receipts = []
            for i in range(3):
                receipts.append(
                    {
                        "blockNumber": "0x1",
                        "transactionHash": f"0x{i+1:064x}",
                        "status": "0x1",
                        "logs": [
                            {
                                "address": "0x0000000000000000000000000000000000000000",
                                "topics": [transfer],
                                "data": "0x",
                            }
                        ],
                    }
                )
            reply(receipts)
            return

        if method != "eth_getLogs":
            reply(None)
            return

        # eth_getLogs: oversize responses for ranges > ok_range; small otherwise.
        flt = params[0] if params else {}
        if not isinstance(flt, dict):
            try:
                flt = json.loads(flt)
            except Exception:
                flt = {}
        fb = flt.get("fromBlock", "0x0")
        tb = flt.get("toBlock", fb)
        start = _hex_to_int(fb)
        end = _hex_to_int(tb)
        rng = (end - start + 1) if end >= start else 0

        if rng <= self.server.ok_range:
            reply([{"blockNumber": str(fb), "data": "0x"}])
            return

        # Stream huge-but-valid JSON without buffering.
        # Each item: {"blockNumber":"0x1","data":"0xaaaa...."}
        # data_hex_len controls payload size; must be even for "0x" hex string.
        data_hex_len = int(self.server.data_hex_len)
        if data_hex_len % 2 != 0:
            data_hex_len += 1
        data_chunk = ("a" * data_hex_len).encode("utf-8")
        item_prefix = b'{"blockNumber":"0x1","data":"0x'
        item_suffix = b'"}'

        item_len = len(item_prefix) + len(data_chunk) + len(item_suffix) + 1  # + comma approx
        target_bytes = int(self.server.oversize_target_bytes)
        items = max(1, (target_bytes // max(1, item_len)) + 1)

        self.send_response(200)
        self.send_header("content-type", "application/json")
        # Help clients avoid downloading maxBytes just to discover it is too large.
        # Compute exact Content-Length for the streamed JSON payload.
        id_bytes = json.dumps(req_id, separators=(",", ":"), ensure_ascii=True).encode("utf-8")
        prefix = b'{"jsonrpc":"2.0","id":' + id_bytes + b',"result":['
        close = b"]}"
        item_bytes_len = len(item_prefix) + len(data_chunk) + len(item_suffix)
        total_len = len(prefix) + (items * item_bytes_len) + max(0, items-1) + len(close)
        self.send_header("content-length", str(total_len))
        self.end_headers()

        # Start object + array
        self.wfile.write(b'{"jsonrpc":"2.0","id":')
        self.wfile.write(id_bytes)
        self.wfile.write(b',"result":[')
        for i in range(items):
            if i:
                self.wfile.write(b",")
            self.wfile.write(item_prefix)
            self.wfile.write(data_chunk)
            self.wfile.write(item_suffix)
        self.wfile.write(b"]}")
        return


def main() -> int:
    ap = argparse.ArgumentParser(description="Mock EVM upstream with oversized eth_getLogs responses.")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=14031)
    ap.add_argument("--chain-id", type=int, default=8453)
    ap.add_argument("--latest-block", default="0x4000000")
    ap.add_argument("--ok-range", type=int, default=25)
    ap.add_argument("--oversize-mb", type=int, default=17, help="Target response size when oversizing (MiB).")
    ap.add_argument("--data-hex-len", type=int, default=2048, help="Hex chars per log.data payload (excludes 0x).")
    args = ap.parse_args()

    srv = HTTPServer((args.host, args.port), Handler)
    srv.chain_id_hex = hex(int(args.chain_id))
    srv.latest_block_hex = str(args.latest_block)
    srv.ok_range = int(args.ok_range)
    srv.oversize_target_bytes = int(args.oversize_mb) * 1024 * 1024
    srv.data_hex_len = int(args.data_hex_len)

    print(f"listening http://{args.host}:{args.port} chain_id={args.chain_id} ok_range={args.ok_range} oversize_mib={args.oversize_mb}", file=sys.stderr)
    srv.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
