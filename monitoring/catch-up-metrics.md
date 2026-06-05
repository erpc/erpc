# Catch-up metrics — how to read them

A reasoning guide for the **Catch-up Retries / Wait / Pressure** panels (Grafana
row *Networks – Advanced*). Read this before concluding anything from those
panels — a non-zero baseline is **normal**, and the panel that looks scariest
(p95 = 10 s) is usually the healthiest signal.

## TL;DR

erpc deliberately waits **~one block** and retries when the selected upstream
doesn't have the requested data *yet* ("catch-up"), instead of failing the
request. Three panels expose it:

| panel | metric | answers |
|---|---|---|
| **Catch-up Retries by Reason** | `rate(network_retry_attempt_total)` | how *often* catch-up fires, by reason & network |
| **Catch-up Wait — p95** | `histogram_quantile(0.95, rate(..._wait_seconds_bucket))` | how *long* each wait is — expect ≈ one block time |
| **Catch-up Wait Pressure** | `rate(..._wait_seconds_sum)` | the *impact*: ≈ how many requests are sitting in a catch-up wait right now |

Healthy = per-retry wait ≈ the chain's block time, and pressure low/flat.
**Act** when pressure climbs unbounded, p95 ≫ one block, or catch-up fires on
`finalized` data.

## What "catch-up" actually is

A request asks for data at the chain tip — `eth_getBlockByNumber(latest)`,
logs in the newest block, a just-broadcast receipt. The upstream that gets
picked may not have indexed that block yet (it's a second behind the tip, or
returns `null`/`[]` for a block it hasn't ingested). Rather than surface an
empty/error to the caller, erpc treats this as *"data not available **yet**"*
and does a **block-time-relative wait, then retries** — betting the data
appears within ~one block. This is fundamentally different from **genuine-error
failover** (a 5xx / timeout / dead upstream), which retries on **exponential
backoff**, not a block-time wait.

Code: `networkExecutor.computeDelay` / `shouldRetryWithReason` /
`isDataUnavailableReason` in `erpc/network_executor.go`.

## The two metrics (count + duration, same `reason` label)

Both are network-scope and share labels `{project, network, category, reason, finality}`
(`category` = the RPC method; historically named).

- **`erpc_network_retry_attempt_total`** — counter. Every network-scope retry,
  by `reason`. The **count** side.
- **`erpc_network_data_unavailable_wait_seconds`** — histogram
  (`_bucket`/`_sum`/`_count`). The wall-clock delay **deliberately** spent
  waiting before a data-not-yet-available retry. The **duration** side.
  Recorded **only** for the catch-up reasons below.

### Reasons

| reason | meaning | catch-up wait? |
|---|---|---|
| `block_unavailable` | upstream explicitly said it doesn't have that block yet (`ErrUpstreamBlockUnavailable`) — request is ahead of the upstream's head | ✅ block-time wait |
| `empty_result` | upstream returned emptyish (`null` block, `[]` logs, empty receipt) for a recent block, with `RetryEmpty` set | ✅ block-time wait |
| `missing_data` | point-lookup returned `ErrEndpointMissingData` ("I don't have this") | ✅ block-time wait |
| `pending_tx` | tx-lookup retry (`RetryPending`) hunting an upstream that has the tx | ❌ exponential backoff — **excluded** from the wait histogram |
| `retryable_error` | genuine retryable error (network/5xx) | ❌ exponential backoff (failover) |

Only the first three are "catch-up". `pending_tx`/`retryable_error` show up in
the **Retries** panel (they're retries) but **not** in Wait/Pressure (their cost
is backoff, not chain catch-up — mixing them would mislabel failover as
freshness).

### The `finality` label — read it

- catch-up on `unfinalized` / `realtime` data = **normal** (tip reads race the
  chain).
- catch-up on **`finalized`** data = **red flag**. Finalized data should always
  be present; `empty_result`/`missing_data` there means a genuinely missing
  range or a misconfigured/archive-incomplete upstream — not a timing race.

## How the wait is computed (so you know what "normal" looks like)

`computeDelay` for a catch-up reason returns, in order:

1. **`dynamicBlockUnavailableDelay()`** = EMA-estimated block time ×
   `blockUnavailableDelayMultiplier` (default **1.0**) — i.e. *wait one block*.
   This is the steady-state path once the block-time EMA has warmed up.
2. else **`emptyResultDelay`** — a fixed fallback (default **700 ms**), used
   before the EMA warms up (e.g. a freshly-deployed pod).

Total catch-up retries are bounded by **`emptyResultMaxAttempts`** (default
**2** → one retry), a cap **separate** from `maxAttempts` (genuine-error
failover). So a request takes at most ~one block-time wait before giving up.

**Consequence:** the expected p95 wait is *per-chain* and equals roughly one
block:

| chain (block time) | expected p95 catch-up wait |
|---|---|
| Ethereum mainnet (~12 s) | ~10–12 s |
| Polygon / Base / Optimism / Avalanche (~2 s) | ~2 s |
| BSC (~3 s) | ~3 s |
| Arbitrum One (~0.25 s) | ~250–500 ms (or the 700 ms cold fallback) |

A 10 s p95 on **mainnet** is therefore *correct* — it's one Ethereum block. The
same 10 s on Arbitrum would be alarming.

## Reading the panels (decision tree)

1. **Retries by Reason** — is catch-up firing, what reason, which network?
   Baseline non-zero is fine. Look at the *mix*:
   - `block_unavailable` heavy on a fast chain → the picked upstream's head lags
     the tip (cross-check the selection policy / `block_head_lag`).
   - `empty_result` / `missing_data` → upstream returns empty for recent blocks
     (indexing lag) **or** the data is legitimately empty and `RetryEmpty` is
     over-eager.
2. **Wait p95** — is each wait ≈ one block for that chain (table above)?
   - p95 ≫ one block → the EMA block-time estimate is inflated (a laggy upstream
     dragging the EMA), or backoff is leaking onto this path.
   - p95 pinned at exactly 700 ms → EMA never warmed (cold pods / churn);
     everything's on the fixed fallback.
3. **Wait Pressure** — the only panel that measures *impact*. `rate(_sum)` is
   wait-seconds accrued per second ≈ **average number of requests concurrently
   blocked in a catch-up wait** (Little's law: `concurrency = rate × duration`).
   - < 1 → negligible; a handful of tip reads waiting. Ignore.
   - high & **rising / unbounded** → many requests stuck waiting = an upstream
     freshness problem, or you're hammering the tip faster than upstreams index.
     This is what actually adds latency to user requests.
   - Note pressure scales with *wait length*: a slow chain (10 s waits) reaches
     pressure 1.0 at just 0.1 retries/s, while Arbitrum (0.4 s waits) needs
     ~2.5 retries/s. Pressure normalizes "how often" against "how costly".

## Healthy vs. act-now

**Healthy:** flat/low pressure; p95 ≈ one block per chain; reason mix matches
chain speed; all on `unfinalized` finality; brief regime shifts right after a
deploy (cold EMA → 700 ms fallback until warm) that settle within a minute.

**Act:**
- Pressure climbing without a ceiling on a network → upstreams chronically
  behind tip. Check the selection policy (is it routing to a laggy primary?) and
  upstream `block_head_lag`.
- p95 wait ≫ one block on a fast chain → EMA inflated by a bad upstream; or the
  multiplier is set too high.
- Retries high **and** hitting `emptyResultMaxAttempts` → requests exhaust
  catch-up and surface empties/errors to callers.
- **Any** catch-up volume on `finalized` finality → investigate the upstream
  (missing range / incomplete archive), not the timing.

## Tuning knobs (`retry` / `evm` config)

| knob | path | effect |
|---|---|---|
| `blockUnavailableDelayMultiplier` | `evm` (default 1.0) | scales the per-block wait. <1 = fail faster, less latency, more empties; >1 = wait longer, fewer empties, more latency. |
| `emptyResultDelay` | `retry` (default 700 ms) | the cold fallback wait before the block-time EMA warms up. |
| `emptyResultMaxAttempts` | `retry` (default 2) | how many catch-up retries before giving up. Separate from `maxAttempts`. |

## Worked example

A typical snapshot of the three panels:

- **Retries**: `mainnet / missing_data` and `base / block_unavailable` dominate,
  `arbitrum-one / empty_result` rising — ~2–3 retries/s total. Reason mix tracks
  chain speed (fast L2s race the tip via `block_unavailable`; mainnet sees
  `missing_data`/`empty_result` on recent blocks). **Normal.**
- **Wait p95**: `mainnet / empty_result` = 10 s, everything else ≈ 480 ms.
  The 10 s is *one Ethereum block* — expected, not a stall. The sub-second
  values are one block on the L2s (or the cold fallback). **Healthy.**
- **Pressure**: peaks around ~1.0 (≈ one request continuously waiting) on
  `arbitrum-one / empty_result`. Low impact — **not alarming**. If it kept
  climbing past a few, *then* you'd chase upstream freshness.
- A regime shift where the reason mix flips within a minute usually lines up
  with a **deploy**: new pods start with a cold block-time EMA and fall back to
  `emptyResultDelay` until it warms, briefly changing both the dominant reason
  and the wait length. Correlate with release time before suspecting an upstream.

**Bottom line:** catch-up firing is the system doing its job (trading a
sub-block wait for fresher, non-empty responses). Judge it by **pressure trend**
and **per-chain wait sanity**, not by raw retry counts or a scary-looking
slow-chain p95.
