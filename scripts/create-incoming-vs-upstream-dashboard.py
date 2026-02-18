#!/usr/bin/env python3
"""
Create the "eRPC Incoming vs Upstream" Grafana dashboard.

Compares incoming requests (what users send to eRPC) vs upstream requests
(what eRPC sends to RPC providers) to reveal the amplification/reduction factor.

Usage:
    python3 scripts/create-incoming-vs-upstream-dashboard.py

Requires GRAFANA_TOKEN env var (or uses the default service account token).
"""

import json
import os
import sys
import urllib.request
import urllib.error

GRAFANA_URL = "https://monitoring.morpho.dev"
GRAFANA_TOKEN = os.environ.get("GRAFANA_TOKEN", "")
DASHBOARD_UID = "erpc-incoming-vs-upstream"
DASHBOARD_TITLE = "eRPC Incoming vs Upstream"
FOLDER_UID = "erpc-alerts"
TAGS = ["erpc", "requests", "upstream"]

# Common label filters applied to most queries
NETWORK_FILTERS = (
    'project=~"${project:regex}", '
    'network=~"${network:regex}", '
    'user=~"${user:regex}", '
    'category=~"${category:regex}", '
    'finality=~"${finality:regex}"'
)
UPSTREAM_FILTERS = (
    'project=~"${project:regex}", '
    'vendor=~"${vendor:regex}", '
    'network=~"${network:regex}", '
    'user=~"${user:regex}", '
    'category=~"${category:regex}", '
    'finality=~"${finality:regex}", '
    'composite="none"'
)
CACHE_FILTERS = (
    'project=~"${project:regex}", '
    'network=~"${network:regex}", '
    'user=~"${user:regex}", '
    'category=~"${category:regex}"'
)

# Auto-incrementing panel ID
_panel_id = 0


def next_id():
    global _panel_id
    _panel_id += 1
    return _panel_id


# -- Template variables -------------------------------------------------------

def make_datasource_var():
    return {
        "current": {},
        "includeAll": False,
        "name": "datasource",
        "options": [],
        "query": "prometheus",
        "refresh": 1,
        "regex": "",
        "type": "datasource",
    }


def make_query_var(name, label_query, all_value=".*"):
    return {
        "allValue": all_value,
        "current": {},
        "datasource": {"type": "prometheus", "uid": "${datasource}"},
        "definition": label_query,
        "includeAll": True,
        "multi": True,
        "name": name,
        "options": [],
        "query": {
            "qryType": 1,
            "query": label_query,
            "refId": "PrometheusVariableQueryEditor-VariableQuery",
        },
        "refresh": 1,
        "regex": "",
        "type": "query",
    }


def make_interval_var():
    return {
        "auto": True,
        "auto_count": 30,
        "auto_min": "10s",
        "current": {"selected": False, "text": "auto", "value": "$__auto_interval_interval"},
        "name": "interval",
        "options": [
            {"selected": True, "text": "auto", "value": "$__auto_interval_interval"},
            {"selected": False, "text": "1m", "value": "1m"},
            {"selected": False, "text": "5m", "value": "5m"},
            {"selected": False, "text": "15m", "value": "15m"},
            {"selected": False, "text": "1h", "value": "1h"},
        ],
        "query": "1m,5m,15m,1h",
        "refresh": 2,
        "type": "interval",
    }


TEMPLATE_VARS = [
    make_datasource_var(),
    make_query_var("project", "label_values(project)"),
    make_query_var("network", "label_values(network)"),
    make_query_var("vendor", "label_values(vendor)"),
    make_query_var("user", "label_values(user)"),
    make_query_var("category", "label_values(category)"),
    make_query_var("finality", "label_values(finality)"),
    make_interval_var(),
]


# -- Panel builders -----------------------------------------------------------

def datasource_ref():
    return {"type": "prometheus", "uid": "${datasource}"}


def target(expr, legend, ref="A"):
    return {
        "datasource": datasource_ref(),
        "editorMode": "code",
        "expr": expr,
        "legendFormat": legend,
        "range": True,
        "refId": ref,
    }


def timeseries_defaults(unit="ops", fill=10, stacking="none", min_val=None, max_val=None):
    d = {
        "color": {"mode": "palette-classic"},
        "custom": {
            "axisBorderShow": False,
            "axisCenteredZero": False,
            "axisColorMode": "text",
            "axisLabel": "",
            "axisPlacement": "auto",
            "barAlignment": 0,
            "barWidthFactor": 0.8,
            "drawStyle": "line",
            "fillOpacity": fill,
            "gradientMode": "none",
            "hideFrom": {"legend": False, "tooltip": False, "viz": False},
            "insertNulls": False,
            "lineInterpolation": "smooth",
            "lineWidth": 2,
            "pointSize": 5,
            "scaleDistribution": {"type": "linear"},
            "showPoints": "never",
            "spanNulls": True,
            "stacking": {"group": "A", "mode": stacking},
            "thresholdsStyle": {"mode": "off"},
        },
        "mappings": [],
        "thresholds": {
            "mode": "absolute",
            "steps": [{"color": "green"}],
        },
        "unit": unit,
    }
    if min_val is not None:
        d["min"] = min_val
    if max_val is not None:
        d["max"] = max_val
    return d


def timeseries_panel(title, targets, x, y, w=12, h=8, unit="ops",
                     fill=10, stacking="none", description="",
                     min_val=None, max_val=None, overrides=None):
    panel = {
        "datasource": datasource_ref(),
        "description": description,
        "fieldConfig": {
            "defaults": timeseries_defaults(unit, fill, stacking, min_val, max_val),
            "overrides": overrides or [],
        },
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "id": next_id(),
        "options": {
            "legend": {
                "calcs": ["mean", "lastNotNull"],
                "displayMode": "table",
                "placement": "bottom",
                "showLegend": True,
            },
            "tooltip": {"hideZeros": False, "mode": "multi", "sort": "desc"},
        },
        "pluginVersion": "12.0.1",
        "targets": targets,
        "title": title,
        "type": "timeseries",
    }
    return panel


def stat_panel(title, targets, x, y, w=4, h=4, unit="ops", description="",
               color_mode="background_solid", thresholds=None):
    if thresholds is None:
        thresholds = {
            "mode": "absolute",
            "steps": [{"color": "green"}],
        }
    return {
        "datasource": datasource_ref(),
        "description": description,
        "fieldConfig": {
            "defaults": {
                "color": {"mode": thresholds and "thresholds" or "palette-classic"},
                "mappings": [],
                "thresholds": thresholds,
                "unit": unit,
            },
            "overrides": [],
        },
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "id": next_id(),
        "options": {
            "colorMode": color_mode,
            "graphMode": "area",
            "justifyMode": "auto",
            "orientation": "auto",
            "percentChangeColorMode": "standard",
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "showPercentChange": False,
            "textMode": "auto",
            "wideLayout": True,
        },
        "pluginVersion": "12.0.1",
        "targets": targets,
        "title": title,
        "type": "stat",
    }


def row_panel(title, y, collapsed=False, panels=None):
    r = {
        "collapsed": collapsed,
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": y},
        "id": next_id(),
        "title": title,
        "type": "row",
    }
    if collapsed and panels:
        r["panels"] = panels
    return r


# -- Build all panels ----------------------------------------------------------

def build_panels():
    panels = []
    y = 0

    # =========================================================================
    # Row 1: Overview
    # =========================================================================
    panels.append(row_panel("Overview", y))
    y += 1

    # Stat panels (3 across the top)
    panels.append(stat_panel(
        "Incoming req/s",
        [target(
            f'sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval]))',
            "Incoming",
        )],
        x=0, y=y, w=8, h=4, unit="reqps",
        description="Total incoming requests per second across all networks.",
    ))
    panels.append(stat_panel(
        "Upstream req/s",
        [target(
            f'sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval]))',
            "Upstream",
        )],
        x=8, y=y, w=8, h=4, unit="reqps",
        description="Total upstream requests per second (excluding composite/multicall3 synthetic requests).",
    ))
    panels.append(stat_panel(
        "Reduction %",
        [target(
            f'1 - (sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) / sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])))',
            "Reduction",
        )],
        x=16, y=y, w=8, h=4, unit="percentunit",
        description="Percentage of incoming requests that DON'T need an upstream call.",
        thresholds={
            "mode": "absolute",
            "steps": [
                {"color": "red"},
                {"color": "yellow", "value": 0.3},
                {"color": "green", "value": 0.6},
            ],
        },
    ))
    y += 4

    # Incoming vs Upstream (stacked area)
    incoming_override = {
        "matcher": {"id": "byRegexp", "options": "Incoming.*"},
        "properties": [
            {"id": "color", "value": {"fixedColor": "blue", "mode": "fixed"}},
        ],
    }
    upstream_override = {
        "matcher": {"id": "byRegexp", "options": "Upstream.*"},
        "properties": [
            {"id": "color", "value": {"fixedColor": "orange", "mode": "fixed"}},
        ],
    }
    panels.append(timeseries_panel(
        "Incoming vs Upstream Requests",
        [
            target(
                f'sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval]))',
                "Incoming",
            ),
            target(
                f'sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval]))',
                "Upstream", ref="B",
            ),
        ],
        x=0, y=y, w=12, h=8,
        description="Incoming requests (what users send) vs upstream requests (what hits providers). The gap is the reduction from caching, multiplexing, and multicall3.",
        overrides=[incoming_override, upstream_override],
    ))

    # Reduction ratio over time
    panels.append(timeseries_panel(
        "Request Reduction Ratio",
        [target(
            f'1 - (sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) / sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])))',
            "Reduction ratio",
        )],
        x=12, y=y, w=12, h=8, unit="percentunit",
        description="Fraction of incoming requests that don't result in an upstream call (0=all forwarded, 1=all absorbed).",
        min_val=0, max_val=1,
    ))
    y += 8

    # =========================================================================
    # Row 2: Breakdown by User
    # =========================================================================
    panels.append(row_panel("Breakdown by User", y))
    y += 1

    panels.append(timeseries_panel(
        "Incoming Requests by User",
        [target(
            f'sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])) by (user)',
            "{{user}}",
        )],
        x=0, y=y, w=8, h=8,
        description="Incoming request rate grouped by user.",
    ))
    panels.append(timeseries_panel(
        "Upstream Requests by User",
        [target(
            f'sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (user)',
            "{{user}}",
        )],
        x=8, y=y, w=8, h=8,
        description="Upstream request rate grouped by user (composite=none).",
    ))
    panels.append(timeseries_panel(
        "Reduction Ratio by User",
        [target(
            f'1 - (sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (user) / sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])) by (user))',
            "{{user}}",
        )],
        x=16, y=y, w=8, h=8, unit="percentunit",
        description="Per-user reduction ratio (how much each user benefits from caching/multiplexing).",
        min_val=0, max_val=1,
    ))
    y += 8

    # =========================================================================
    # Row 3: Breakdown by Network
    # =========================================================================
    panels.append(row_panel("Breakdown by Network", y))
    y += 1

    panels.append(timeseries_panel(
        "Incoming vs Upstream by Network",
        [
            target(
                f'sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])) by (network)',
                "IN {{network}}",
            ),
            target(
                f'sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (network)',
                "UP {{network}}", ref="B",
            ),
        ],
        x=0, y=y, w=12, h=8,
        description="Incoming (IN) and upstream (UP) request rates per network.",
    ))

    # Cache hit rate by network
    panels.append(timeseries_panel(
        "Cache Hit Rate by Network",
        [target(
            f'sum(rate(erpc_cache_get_success_hit_total{{{CACHE_FILTERS}}}[$__rate_interval])) by (network) / (sum(rate(erpc_cache_get_success_hit_total{{{CACHE_FILTERS}}}[$__rate_interval])) by (network) + sum(rate(erpc_cache_get_success_miss_total{{{CACHE_FILTERS}}}[$__rate_interval])) by (network))',
            "{{network}}",
        )],
        x=12, y=y, w=12, h=8, unit="percentunit",
        description="Cache hit rate per network (hits / (hits + misses)).",
        min_val=0, max_val=1,
    ))
    y += 8

    # =========================================================================
    # Row 4: Breakdown by Category (method)
    # =========================================================================
    panels.append(row_panel("Breakdown by Category", y))
    y += 1

    panels.append(timeseries_panel(
        "Incoming Requests by Category",
        [target(
            f'sum(rate(erpc_network_request_received_total{{{NETWORK_FILTERS}}}[$__rate_interval])) by (category)',
            "{{category}}",
        )],
        x=0, y=y, w=12, h=8,
        description="Incoming request rate grouped by method category.",
    ))
    panels.append(timeseries_panel(
        "Upstream Requests by Category",
        [target(
            f'sum(rate(erpc_upstream_request_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (category)',
            "{{category}}",
        )],
        x=12, y=y, w=12, h=8,
        description="Upstream request rate grouped by method category (composite=none).",
    ))
    y += 8

    # =========================================================================
    # Row 5: Where Requests Get Absorbed
    # =========================================================================
    panels.append(row_panel("Where Requests Get Absorbed", y))
    y += 1

    panels.append(timeseries_panel(
        "Cache Hits",
        [target(
            f'sum(rate(erpc_cache_get_success_hit_total{{{CACHE_FILTERS}}}[$__rate_interval])) by (network)',
            "{{network}}",
        )],
        x=0, y=y, w=8, h=8,
        description="Rate of cache hits — requests served from cache without hitting upstream.",
    ))
    panels.append(timeseries_panel(
        "Multiplexed Requests",
        [target(
            f'sum(rate(erpc_network_multiplexed_request_total{{{NETWORK_FILTERS}}}[$__rate_interval])) by (network)',
            "{{network}}",
        )],
        x=8, y=y, w=8, h=8,
        description="Rate of multiplexed (deduplicated) requests — identical in-flight requests collapsed into one upstream call.",
    ))
    panels.append(timeseries_panel(
        "Upstream Errors & Skips",
        [
            target(
                f'sum(rate(erpc_upstream_request_errors_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (network)',
                "errors {{network}}",
            ),
            target(
                f'sum(rate(erpc_upstream_request_skipped_total{{{UPSTREAM_FILTERS}}}[$__rate_interval])) by (network)',
                "skipped {{network}}", ref="B",
            ),
        ],
        x=16, y=y, w=8, h=8,
        description="Upstream errors and skipped requests by network.",
    ))
    y += 8

    return panels


# -- Assemble dashboard --------------------------------------------------------

def build_dashboard():
    return {
        "__inputs": [],
        "__elements": {},
        "__requires": [
            {"type": "grafana", "id": "grafana", "name": "Grafana", "version": "12.0.1"},
            {"type": "datasource", "id": "prometheus", "name": "Prometheus", "version": "1.0.0"},
            {"type": "panel", "id": "stat", "name": "Stat", "version": ""},
            {"type": "panel", "id": "timeseries", "name": "Time series", "version": ""},
        ],
        "annotations": {
            "list": [{
                "builtIn": 1,
                "datasource": {"type": "grafana", "uid": "-- Grafana --"},
                "enable": True,
                "hide": True,
                "iconColor": "rgba(0, 211, 255, 1)",
                "name": "Annotations & Alerts",
                "type": "dashboard",
            }]
        },
        "editable": True,
        "fiscalYearStartMonth": 0,
        "graphTooltip": 1,  # shared crosshair
        "id": None,
        "links": [],
        "panels": build_panels(),
        "schemaVersion": 40,
        "tags": TAGS,
        "templating": {"list": TEMPLATE_VARS},
        "time": {"from": "now-6h", "to": "now"},
        "timepicker": {},
        "timezone": "",
        "title": DASHBOARD_TITLE,
        "uid": DASHBOARD_UID,
        "version": 0,
    }


# -- Grafana API ---------------------------------------------------------------

def post_dashboard(dashboard_json):
    payload = {
        "dashboard": dashboard_json,
        "folderUid": FOLDER_UID,
        "overwrite": True,
    }
    data = json.dumps(payload).encode("utf-8")

    req = urllib.request.Request(
        f"{GRAFANA_URL}/api/dashboards/db",
        data=data,
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {GRAFANA_TOKEN}",
        },
        method="POST",
    )

    try:
        with urllib.request.urlopen(req) as resp:
            result = json.loads(resp.read().decode())
            print(f"Dashboard created/updated successfully!")
            print(f"  URL: {GRAFANA_URL}{result.get('url', '')}")
            print(f"  UID: {result.get('uid', '')}")
            print(f"  Version: {result.get('version', '')}")
            return result
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        print(f"Error {e.code}: {body}", file=sys.stderr)
        sys.exit(1)


def save_json(dashboard_json, path):
    with open(path, "w") as f:
        json.dump(dashboard_json, f, indent=2)
        f.write("\n")
    print(f"Dashboard JSON saved to {path}")


# -- Main ----------------------------------------------------------------------

if __name__ == "__main__":
    dashboard = build_dashboard()

    # Always save JSON locally
    out_path = "monitoring/grafana/dashboards/incoming-vs-upstream.json"
    save_json(dashboard, out_path)

    # POST to Grafana if token is available
    if GRAFANA_TOKEN:
        post_dashboard(dashboard)
    else:
        print("\nNo GRAFANA_TOKEN set — skipping API upload.")
        print(f"Import the JSON file manually: {out_path}")
        print(f"Or re-run with: GRAFANA_TOKEN=<token> python3 {sys.argv[0]}")
