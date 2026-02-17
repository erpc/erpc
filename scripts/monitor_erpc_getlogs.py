#!/usr/bin/env python3
import argparse
import json
import os
import sys
import time
import urllib.parse
import urllib.request


def _prom_query(base_url: str, promql: str, timeout_s: float) -> float | None:
    qs = urllib.parse.urlencode({"query": promql})
    url = f"{base_url.rstrip('/')}/api/v1/query?{qs}"
    req = urllib.request.Request(url=url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout_s) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return None

    if not isinstance(data, dict) or data.get("status") != "success":
        return None
    r = (data.get("data") or {}).get("result") or []
    if not r:
        return 0.0
    v = r[0].get("value")
    if not v or len(v) < 2:
        return None
    try:
        return float(v[1])
    except Exception:
        return None


def _prom_query_vector(base_url: str, promql: str, timeout_s: float) -> list[dict]:
    qs = urllib.parse.urlencode({"query": promql})
    url = f"{base_url.rstrip('/')}/api/v1/query?{qs}"
    req = urllib.request.Request(url=url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout_s) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return []
    if not isinstance(data, dict) or data.get("status") != "success":
        return []
    r = (data.get("data") or {}).get("result") or []
    if not isinstance(r, list):
        return []
    out = []
    for it in r:
        try:
            metric = it.get("metric") or {}
            value = it.get("value") or []
            out.append(
                {
                    "metric": metric,
                    "value": float(value[1]) if len(value) >= 2 else None,
                }
            )
        except Exception:
            continue
    return out


def _fmt_b(v: float | None) -> str | None:
    if v is None:
        return None
    if v <= 0:
        return "0B"
    units = ["B", "KiB", "MiB", "GiB", "TiB"]
    x = float(v)
    i = 0
    while x >= 1024.0 and i < len(units) - 1:
        x /= 1024.0
        i += 1
    if i == 0:
        return f"{int(x)}{units[i]}"
    return f"{x:.1f}{units[i]}"


def main() -> int:
    ap = argparse.ArgumentParser(description="Monitor eRPC eth_getLogs perf + errors + memory via Prometheus API (Thanos).")
    ap.add_argument("--thanos", default=os.environ.get("THANOS_URL", "http://127.0.0.1:19090"))
    ap.add_argument("--timeout", type=float, default=float(os.environ.get("PROM_TIMEOUT_S", "5")))
    ap.add_argument("--namespace", default=os.environ.get("ERPC_NS", "morpho-prd"))
    ap.add_argument("--pod-re", default=os.environ.get("ERPC_POD_RE", "erpc-.*"))
    ap.add_argument("--container", default=os.environ.get("ERPC_CONTAINER", "erpc"))
    ap.add_argument(
        "--category",
        default=os.environ.get("ERPC_CATEGORY", "eth_getLogs"),
        help='RPC category label value (default eth_getLogs). Use "" to aggregate all categories.',
    )
    ap.add_argument("--interval", type=float, default=float(os.environ.get("INTERVAL_S", "30")))
    ap.add_argument("--duration", type=float, default=float(os.environ.get("DURATION_S", "600")))
    ap.add_argument("--topk", type=int, default=int(os.environ.get("TOPK", "5")))
    args = ap.parse_args()

    ns = args.namespace
    pod_re = args.pod_re
    c = args.container
    cat = args.category

    # Memory baseline: compare current to recent hours.
    mem_sel = f'container_memory_working_set_bytes{{namespace="{ns}",container="{c}",pod=~"{pod_re}"}}'
    mem_now_q = f"max({mem_sel})"
    mem_baseline_1h_q = f"max(avg_over_time({mem_sel}[1h]))"
    mem_baseline_6h_q = f"max(avg_over_time({mem_sel}[6h]))"
    mem_peak_1h_q = f"max(max_over_time({mem_sel}[1h]))"
    mem_peak_6h_q = f"max(max_over_time({mem_sel}[6h]))"

    cat_sel = f'{{category="{cat}"}}' if cat else ""
    cat_sel_user = f'{{category="{cat}"}}' if cat else ""

    rps_q = f"sum(rate(erpc_network_request_duration_seconds_count{cat_sel}[5m]))"
    err_q = f"sum(rate(erpc_network_failed_request_total{cat_sel}[5m]))"
    p95_q = (
        f"histogram_quantile(0.95, sum by (le) (rate(erpc_network_request_duration_seconds_bucket{cat_sel}[5m])))"
    )
    users_q = f"topk({args.topk}, sum by (user) (rate(erpc_network_request_duration_seconds_count{cat_sel_user}[5m])))"
    users_err_q = f"topk({args.topk}, sum by (user) (rate(erpc_network_failed_request_total{cat_sel_user}[5m])))"

    # Pod health signals.
    restarts_10m_q = (
        f'sum(increase(kube_pod_container_status_restarts_total{{namespace="{ns}",pod=~"{pod_re}",container="{c}"}}[10m]))'
    )
    oom_10m_q = (
        f'sum(max_over_time(kube_pod_container_status_last_terminated_reason{{namespace="{ns}",pod=~"{pod_re}",container="{c}",reason="OOMKilled"}}[10m]))'
    )

    baseline = {
        "mem_avg_1h": _prom_query(args.thanos, mem_baseline_1h_q, args.timeout),
        "mem_avg_6h": _prom_query(args.thanos, mem_baseline_6h_q, args.timeout),
        "mem_peak_1h": _prom_query(args.thanos, mem_peak_1h_q, args.timeout),
        "mem_peak_6h": _prom_query(args.thanos, mem_peak_6h_q, args.timeout),
    }
    print(
        json.dumps(
            {
                "type": "baseline",
                "mem_avg_1h": _fmt_b(baseline["mem_avg_1h"]),
                "mem_avg_6h": _fmt_b(baseline["mem_avg_6h"]),
                "mem_peak_1h": _fmt_b(baseline["mem_peak_1h"]),
                "mem_peak_6h": _fmt_b(baseline["mem_peak_6h"]),
            },
            separators=(",", ":"),
        ),
        flush=True,
    )

    end_t = time.time() + max(0.0, args.duration)
    while True:
        now = time.time()
        if args.duration > 0 and now > end_t:
            break

        mem_now = _prom_query(args.thanos, mem_now_q, args.timeout)
        rps = _prom_query(args.thanos, rps_q, args.timeout)
        err = _prom_query(args.thanos, err_q, args.timeout)
        p95 = _prom_query(args.thanos, p95_q, args.timeout)
        restarts_10m = _prom_query(args.thanos, restarts_10m_q, args.timeout)
        oom_10m = _prom_query(args.thanos, oom_10m_q, args.timeout)

        users = _prom_query_vector(args.thanos, users_q, args.timeout)
        users_err = _prom_query_vector(args.thanos, users_err_q, args.timeout)

        ratio = None
        if rps is not None and err is not None and rps > 0:
            ratio = err / rps

        top_users = []
        for it in users:
            u = (it.get("metric") or {}).get("user") or "<none>"
            v = it.get("value")
            if v is None:
                continue
            top_users.append({"user": u, "rps": round(float(v), 3)})

        top_err_users = []
        for it in users_err:
            u = (it.get("metric") or {}).get("user") or "<none>"
            v = it.get("value")
            if v is None:
                continue
            top_err_users.append({"user": u, "err_rps": round(float(v), 6)})

        print(
            json.dumps(
                {
                    "type": "tick",
                    "ts": int(now),
                    "mem_now": _fmt_b(mem_now),
                    "mem_vs_avg_1h": (
                        None
                        if mem_now is None or baseline["mem_avg_1h"] in (None, 0.0)
                        else round(mem_now / baseline["mem_avg_1h"], 3)
                    ),
                    "mem_vs_avg_6h": (
                        None
                        if mem_now is None or baseline["mem_avg_6h"] in (None, 0.0)
                        else round(mem_now / baseline["mem_avg_6h"], 3)
                    ),
                    "rps": None if rps is None else round(float(rps), 3),
                    "err_rps": None if err is None else round(float(err), 6),
                    "err_ratio": None if ratio is None else round(float(ratio), 8),
                    "p95_s": None if p95 is None else round(float(p95), 4),
                    "restarts_10m": None if restarts_10m is None else int(round(float(restarts_10m))),
                    "oom_10m": None if oom_10m is None else int(round(float(oom_10m))),
                    "top_users": top_users,
                    "top_err_users": top_err_users,
                },
                separators=(",", ":"),
            ),
            flush=True,
        )

        time.sleep(max(0.1, args.interval))

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
