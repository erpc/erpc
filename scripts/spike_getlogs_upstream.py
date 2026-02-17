#!/usr/bin/env python3
import argparse
import json
import os
import random
import re
import sys
import time
import urllib.parse
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed


def _hex_to_int(s: str) -> int:
    s = s.strip().lower()
    if s.startswith("0x"):
        return int(s, 16)
    return int(s)


def _int_to_hex(v: int) -> str:
    if v < 0:
        v = 0
    return hex(v)


def _safe_url(url: str) -> str:
    # Redact secret query parameter for logs.
    u = urllib.parse.urlsplit(url)
    q = urllib.parse.parse_qsl(u.query, keep_blank_values=True)
    q2 = []
    for k, v in q:
        if k.lower() == "secret":
            q2.append((k, "<redacted>"))
        else:
            q2.append((k, v))
    return urllib.parse.urlunsplit((u.scheme, u.netloc, u.path, urllib.parse.urlencode(q2), u.fragment))

def _sanitize_err(s: str) -> str:
    # Avoid leaking secrets if exception messages include the URL.
    if not s:
        return s
    return re.sub(r"(secret=)[^&\\s]+", r"\\1<redacted>", s)


def do_one(
    url: str,
    topic0: str,
    base_from: int,
    span_blocks: int,
    jitter_blocks: int,
    timeout_s: float,
    read_chunk: int,
    use_upstream: str,
    skip_cache_read: bool,
) -> dict:
    # Randomize fromBlock to keep requests distinct (avoid accidental caching effects).
    start = base_from + random.randint(-jitter_blocks, jitter_blocks)
    if start < 0:
        start = 0
    end = start + span_blocks

    body = {
        "method": "eth_getLogs",
        "params": [
            {
                "topics": [topic0],
                "fromBlock": _int_to_hex(start),
                "toBlock": _int_to_hex(end),
            }
        ],
    }

    req = urllib.request.Request(
        url=url,
        method="POST",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "content-type": "application/json",
            # Optional: bypass cache read (still allows cache set).
            **({"X-ERPC-Skip-Cache-Read": "true"} if skip_cache_read else {}),
            # Optional: pin to specific upstream(s) for deterministic runs.
            **({"X-ERPC-Use-Upstream": use_upstream} if use_upstream else {}),
        },
    )

    t0 = time.time()
    downloaded = 0
    status = None
    err = None
    try:
        with urllib.request.urlopen(req, timeout=timeout_s) as resp:
            status = getattr(resp, "status", None) or resp.getcode()
            while True:
                chunk = resp.read(read_chunk)
                if not chunk:
                    break
                downloaded += len(chunk)
    except urllib.error.HTTPError as e:
        # HTTPError is response-like; consume a little so the connection is closed.
        try:
            status = int(getattr(e, "code", None) or 0) or None
        except Exception:
            status = None
        try:
            _ = e.read(min(read_chunk, 64 * 1024))
        except Exception:
            pass
        try:
            e.close()
        except Exception:
            pass
        err = _sanitize_err(f"HTTPError: {status}")
    except Exception as e:
        err = _sanitize_err(str(e))
    dt = time.time() - t0

    return {
        "status": status,
        "seconds": dt,
        "bytes": downloaded,
        "fromBlock": body["params"][0]["fromBlock"],
        "toBlock": body["params"][0]["toBlock"],
        "error": err,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description="Generate eth_getLogs upstream load (skip cache read).")
    ap.add_argument("--url", default=os.environ.get("ERPC_URL", "http://127.0.0.1:14030/cache/evm/8453"))
    ap.add_argument("--secret", default=os.environ.get("ERPC_SECRET", ""))
    ap.add_argument(
        "--secret-stdin",
        action="store_true",
        default=False,
        help="Read secret from stdin (avoids exposing it in args/env).",
    )
    ap.add_argument("--use-upstream", default=os.environ.get("USE_UPSTREAM", ""))
    ap.add_argument("--requests", type=int, default=int(os.environ.get("REQUESTS", "30")))
    ap.add_argument("--concurrency", type=int, default=int(os.environ.get("CONCURRENCY", "3")))
    ap.add_argument("--timeout", type=float, default=float(os.environ.get("TIMEOUT_S", "25")))
    ap.add_argument("--read-chunk", type=int, default=int(os.environ.get("READ_CHUNK", str(256 * 1024))))
    ap.add_argument(
        "--skip-cache-read",
        default=os.environ.get("SKIP_CACHE_READ", "true").lower() == "true",
        action=argparse.BooleanOptionalAction,
        help="Send X-ERPC-Skip-Cache-Read header. Use --no-skip-cache-read to allow cache reads.",
    )
    ap.add_argument("--from-block", default=os.environ.get("FROM_BLOCK", "0x27dd222"))
    ap.add_argument("--span-blocks", type=int, default=int(os.environ.get("SPAN_BLOCKS", "8000")))
    ap.add_argument("--jitter-blocks", type=int, default=int(os.environ.get("JITTER_BLOCKS", "2000")))
    ap.add_argument(
        "--topic0",
        default=os.environ.get(
            "TOPIC0",
            "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",  # ERC20 Transfer
        ),
    )
    args = ap.parse_args()

    if args.secret_stdin:
        # One line, no prompt (caller can provide via pipe or tty).
        args.secret = (sys.stdin.readline() or "").strip()

    if not args.secret:
        print("missing --secret / ERPC_SECRET (or use --secret-stdin)", file=sys.stderr)
        return 2

    # Append secret as query param (don’t print it).
    u = urllib.parse.urlsplit(args.url)
    q = urllib.parse.parse_qsl(u.query, keep_blank_values=True)
    q.append(("secret", args.secret))
    url = urllib.parse.urlunsplit((u.scheme, u.netloc, u.path, urllib.parse.urlencode(q), u.fragment))

    base_from = _hex_to_int(args.from_block)

    print(f"url={_safe_url(url)}", file=sys.stderr, flush=True)
    print(
        f"requests={args.requests} concurrency={args.concurrency} timeout_s={args.timeout} "
        f"from={args.from_block} span_blocks={args.span_blocks} jitter_blocks={args.jitter_blocks}",
        file=sys.stderr,
        flush=True,
    )
    print(f"skip_cache_read={bool(args.skip_cache_read)}", file=sys.stderr, flush=True)
    if args.use_upstream:
        print(f"use_upstream={args.use_upstream}", file=sys.stderr, flush=True)

    ok = 0
    fail = 0
    total_bytes = 0
    dts = []
    status_counts = {}
    err_counts = {}
    t0 = time.time()

    with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        futs = [
            ex.submit(
                do_one,
                url,
                args.topic0,
                base_from,
                args.span_blocks,
                args.jitter_blocks,
                args.timeout,
                args.read_chunk,
                args.use_upstream,
                bool(args.skip_cache_read),
            )
            for _ in range(args.requests)
        ]
        for f in as_completed(futs):
            r = f.result()
            total_bytes += int(r["bytes"] or 0)
            try:
                dts.append(float(r["seconds"]))
            except Exception:
                pass
            st = r.get("status")
            status_counts[st] = status_counts.get(st, 0) + 1
            er = r.get("error")
            if er is not None:
                err_counts[er] = err_counts.get(er, 0) + 1
            if r["error"] is None and r["status"] is not None and 200 <= int(r["status"]) < 300:
                ok += 1
            else:
                fail += 1
            # Minimal per-request line; no secret.
            print(
                json.dumps(
                    {
                        "status": r["status"],
                        "s": round(float(r["seconds"]), 3),
                        "bytes": r["bytes"],
                        "from": r["fromBlock"],
                        "to": r["toBlock"],
                        "error": r["error"],
                    },
                    separators=(",", ":"),
                )
                ,
                flush=True,
            )

    dt = time.time() - t0
    mb = total_bytes / (1024 * 1024)

    def _pct(xs, p):
        if not xs:
            return None
        xs2 = sorted(xs)
        if p <= 0:
            return xs2[0]
        if p >= 100:
            return xs2[-1]
        k = int(round((p / 100.0) * (len(xs2) - 1)))
        if k < 0:
            k = 0
        if k >= len(xs2):
            k = len(xs2) - 1
        return xs2[k]

    p50 = _pct(dts, 50)
    p95 = _pct(dts, 95)
    p99 = _pct(dts, 99)

    top_status = sorted(status_counts.items(), key=lambda kv: (-kv[1], str(kv[0])))[:10]
    top_err = sorted(err_counts.items(), key=lambda kv: (-kv[1], str(kv[0])))[:10]
    top_status_s = ",".join([f"{k}:{v}" for k, v in top_status])
    top_err_s = ",".join([f"{k}:{v}" for k, v in top_err])

    print(
        f"summary ok={ok} fail={fail} downloaded_mib={mb:.1f} wall_s={dt:.1f} "
        f"p50_s={None if p50 is None else round(p50,3)} p95_s={None if p95 is None else round(p95,3)} "
        f"p99_s={None if p99 is None else round(p99,3)}",
        file=sys.stderr,
        flush=True,
    )
    print(f"status_top={top_status_s}", file=sys.stderr, flush=True)
    print(f"errors_top={top_err_s}", file=sys.stderr, flush=True)
    return 0 if fail == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
