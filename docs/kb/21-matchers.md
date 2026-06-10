# KB: Matchers & DSL (method/finality/params matching)

> status: complete
> source-dirs: common/matcher.go, common/match.go, common/static_response.go, common/data.go, common/defaults.go, common/validation.go, common/config.go, data/cache_policy.go, erpc/networks.go, erpc/networks_static_responses.go, upstream/upstream.go, upstream/ratelimiter_budget.go, auth/authorizer.go, erpc/http_server.go, common/tracing_core.go, consensus/quota.go, thirdparty/provider.go

## L1 — Capability (CTO view)

eRPC has a single, uniform pattern engine — `WildcardMatch` — used everywhere a config field accepts a string pattern: cache policies, failsafe rules, rate-limit rules, method allow/ignore lists, aliasing, CORS origins, upstream selectors, tracing matchers, and more. On top of plain globs the engine supports boolean composition (`|` OR, `&` AND, `!` NOT, parentheses for grouping) and hex-numeric range comparisons (`>0xff`, `<=0x1000`, `=0x…`) for EVM param matching. A second function, `MatchesSelector`, extends the same grammar to match an upstream by either its ID or its tags, powering the `use-upstream` directive and served-tip partition grouping. Finality filtering is a parallel axis managed by `SelectExecutor` / `MatchFinalities` with its own 4-tier priority: exact (method+finality) > method-only > finality-only > catch-all.

## L2 — Mechanics (staff-engineer view)

**Core engine (`WildcardMatch`).** Every call tokenizes the pattern string into operators (`|`, `&`, `!`, `(`, `)`) and pattern atoms, then builds a recursive function AST via a Pratt-style recursive-descent parser (`parseOr → parseAnd → parseUnary → parsePrimary`). The resulting `func(string) bool` closure is invoked immediately against the test value. There is no caching of compiled patterns — each call re-tokenizes and re-parses. This is lightweight for short patterns but incurs allocation on hot paths (notably failsafe executor selection on every request). Space between tokens is treated as a separator that flushes the current atom; spaces inside atoms are silently stripped via `strings.TrimSpace` on each token value (`common/matcher.go:L228-L259`).

**Glob atom.** Plain pattern atoms (no operator prefix) are dispatched to `github.com/IGLOU-EU/go-wildcard/v2 v2.1.0` which handles `*` (multi-char wildcard) and `?` (single-char). The `<empty>` literal is handled specially before the glob library: it matches only the empty string (`common/matcher.go:L364-L366`).

**Hex numeric atoms.** When the *value* being tested starts with `0x`, the engine tries numeric comparison operators (`>=`, `<=`, `>`, `<`, `=`) as prefixes on the *pattern*. Both the pattern's threshold and the value are parsed via `parseNumber` which accepts `0x`-prefixed hex (via `strconv.ParseInt(s, 16, 64)`) or plain decimal. Non-hex values are **never tested** numerically — numeric operators on non-`0x` values fall through to glob matching (`common/matcher.go:L373-L391`).

**Boolean composition precedence.** Highest to lowest: NOT (`!`) > AND (`&`) > OR (`|`). Both `&` and `|` are left-associative (standard). This matches Go/C expression precedence. Parentheses override. Two operators adjacent (e.g., `&&`) produce a tokenizer error caught by `ValidatePattern` (`common/matcher.go:L157-L169`).

**`MatchesSelector` and tag awareness.** Built on top of `WildcardMatch`, this function first tests the pattern against the entity's `id`. If that succeeds it returns true immediately. If the pattern contains no `!` operator (purely-positive) it also iterates the entity's `tags []string` and returns true on any tag match. Patterns with any `!` anywhere bypass tag matching entirely to preserve negation semantics — a tag match must never rescue an entity the operator meant to exclude (`common/matcher.go:L73-L100`). `UpstreamMatchesSelector` is the canonical single-call wrapper reading `upstream.Id()` and `upstream.Config().Tags` (`common/matcher.go:L106-L112`).

**`SelectExecutor` — 4-tier priority for failsafe/cache executor dispatch.** Given a slice of typed executors carrying `(MatchMethod, MatchFinality)` fields and the request's `(method, finality)` pair, `SelectExecutor` selects the *first* matching executor in each of four tiers, in priority order (`common/match.go:L18-L78`):
1. **Exact (specific method + specific finality)** — `matchMethod != "*"` AND `len(matchFinality) > 0`
2. **Method-only (specific method, any finality)** — `matchMethod != "*"` AND `len(matchFinality) == 0`
3. **Finality-only (any method, specific finality)** — `matchMethod == "*"` AND `len(matchFinality) > 0`
4. **Catch-all (any method, any finality)** — `matchMethod == "*"` AND `len(matchFinality) == 0`

Within each tier the first matching entry in config order wins. The network-level failsafe executor picker (`erpc/networks.go:L910-L928`) uses an equivalent manual 4-tier loop (not `SelectExecutor`) but has the same semantics. The upstream-level picker (`upstream/upstream.go:L372-L408`) also uses a manual 4-pass loop.

**`MatchFinalities` — set overlap for finality defaults merging.** Empty arrays (nil or len=0) are wildcards: two arrays with at least one empty side always match. Two non-empty arrays match if they share at least one element (any overlap). Used exclusively in `SetDefaults` to identify which default failsafe config to apply to a user-supplied failsafe entry (`common/defaults.go:L17-L34`).

**`matchMethodPattern` (internal).** Used by `SelectExecutor`'s `matchMethodPattern` helper: empty string and `"*"` match anything; exact equality is checked before `WildcardMatch` to avoid regex overhead (`common/match.go:L83-L92`).

**Rate-limit rules.** `RateLimiterBudget.GetRulesByMethod` returns all rules whose `method` field matches the request method via `WildcardMatch` (`upstream/ratelimiter_budget.go:L63-L78`). Multiple rules can apply for a given method (e.g., one global `*` rule and one per-method rule).

**Cache policy matching.** `CachePolicy.MatchesForSet` and `MatchesForGet` both apply `WildcardMatch` to `network` first, then `method`, then `matchParams` in order. `matchParams` iterates the config's `params` array positionally, calling `matchParam` per element. `matchParam` dispatches recursively: map→map by key (all keys in config pattern must match), array→array by length+element, string→string via `WildcardMatch`, other types via `paramToString` equality. `paramToString` converts numbers to decimal strings (`strconv.FormatFloat`), bools to `"true"/"false"` (`data/cache_policy.go:L166-L249`). A `finality` must also match exactly (or "unknown" uses a special relaxed comparison — see L133-L149).

**Static response matching.** `FindStaticResponseMatch` uses exact string equality for `method` (no glob), then `paramsEqual` for params which uses recursive deep equality tolerating numeric type divergence (YAML int vs JSON float64) via `toFloat64` cast (`common/static_response.go:L14-L24`). This is distinct from the cache policy `matchParams` which uses `WildcardMatch` on strings.

**Auth method filters.** In `auth/authorizer.go:shouldApplyToMethod` ignoreMethods are checked first (any match → do NOT apply), then allowMethods (any match → force-apply, overriding any ignore). Both use `WildcardMatch`. This is the same semantics as the upstream `IgnoreMethods`/`AllowMethods` in `upstream.go:L1375-L1399`.

**Error recovery.** `ValidatePattern` wraps the tokenizer/parser with a `recover()` that converts panics to `erpc_unexpected_panic_total{scope="validate-pattern"}` metrics and re-panics non-string recoveries (`common/matcher.go:L114-L134`). At runtime, `WildcardMatch` errors are almost always swallowed (callers `if err != nil { continue }` pattern) so a malformed pattern silently skips the match.

## L3 — Exhaustive reference (agent view)

### Config fields

The matcher engine itself has no dedicated config block — it is invoked directly by other subsystems. The table below covers **every config field in the codebase that is matched via `WildcardMatch`, `MatchesSelector`, `SelectExecutor`, or `MatchFinalities`**. Fields are grouped by subsystem.

#### Failsafe (`FailsafeConfig`, used at network scope, upstream scope, and cache connector scope)

| YAML path | Type | Default | Behavior / notes | Citation |
|-----------|------|---------|------------------|----------|
| `*.failsafe[].matchMethod` | string | `"*"` — `SetDefaults` fills empty with `"*"` if no inherited default | WildcardMatch pattern matched against the request's JSON-RPC method string. `"*"` matches all methods. Empty is rejected by `Validate()` with `"failsafe.matchMethod cannot be empty, use '*' to match any method"`. During defaults inheritance (upstream ↔ `upstreamDefaults`), matched by wildcard of default's matchMethod against the entry's matchMethod. | `common/config.go:L1280`, `common/defaults.go:L2131-L2137`, `common/validation.go:L984-L988` |
| `*.failsafe[].matchFinality` | []DataFinalityState | `[]` (empty = matches any finality) | List of finality states this entry applies to. Empty list = catch-all finality tier. See DataFinalityState values below. Matched by `slices.Contains(fl, finality)` — all listed states are eligible; no wildcard syntax inside the list. | `common/config.go:L1281`, `common/match.go:L38-L41` |

`FailsafeConfig` is shared under: `networks[].failsafe[]`, `upstreamDefaults.failsafe[]`, `upstreams[].failsafe[]`, `networkDefaults.failsafe[]`, `database.evmJsonRpcCache.connectors[].failsafeForGets[]`, `database.evmJsonRpcCache.connectors[].failsafeForSets[]`.

**Legacy single-object format (backward compat).** `failsafe` in YAML may be written as a **single object** (the pre-array format) instead of an array. Three struct types each carry a custom `UnmarshalYAML` that detects the old shape: `NetworkConfig` (`common/config.go:L2041-L2103`), `NetworkDefaults` (`common/config.go:L602-L656`), and `UpstreamConfig` (`common/config.go:L825-L905`, covering both `upstreams[].failsafe` and `upstreamDefaults.failsafe`). The migration logic is identical for all three:
1. First attempts to unmarshal into the canonical type normally (array form). If successful, done.
2. If that fails with an error that is NOT an unknown-field error (i.e., the YAML is structurally the old object, not a typo), attempts to unmarshal into an `oldXxx` shadow struct that has `Failsafe *FailsafeConfig` (a single pointer, not a slice).
3. If the old-form unmarshal succeeds and `old.Failsafe != nil`: if `old.Failsafe.MatchMethod == ""` it is forced to `"*"` for backward compatibility; the single object is wrapped into a one-element slice and assigned.
4. If both unmarshal attempts fail, the original error from attempt 1 is returned (it is usually more informative).

The net effect: any pre-existing `failsafe:` single-object YAML config continues to work exactly as before, and is treated as a single catch-all entry matching all methods. No warning or deprecation log is emitted. This migration happens at YAML parse time — the in-memory representation is always `[]*FailsafeConfig` after load.

**Zero-config baseline (auto-generated 'main' project).** When `len(c.Projects) == 0` (i.e., the user provides a config with no `projects:` key at all), `Config.SetDefaults` automatically creates a single project with `id: "main"` (`common/defaults.go:L100-L167`). This auto-generated project carries the following pre-filled failsafe entries (exact values; these are NOT individually documented defaults — they are hard-coded in this branch only):

- `networkDefaults.failsafe`:
  ```
  - matchMethod: "*"
    retry:
      maxAttempts: 5
      delay: 0
      jitter: 0
      backoffMaxDelay: 0
      backoffFactor: 1.0
    timeout:
      duration: 120s
    hedge:
      delay: {quantile: 0.7}   # adaptive hedge at p70 latency
      maxCount: 2
  ```
- `upstreamDefaults.failsafe`:
  ```
  - matchMethod: "*"
    retry:
      maxAttempts: 1
      delay: 500ms
    timeout:
      duration: 60s
  ```

These defaults are NOT applied when the user defines any project (even an empty one). They exist only to give a sensible out-of-the-box experience for zero-config setups (e.g., `erpc` binary run with no config file or a minimal one that only configures upstreams at the server level). The auto-generated project also sets `server.aliasing` to a wildcard domain rule (`matchDomain: "*"`, `serveProject: "main"`) so requests to any path are routed to the project. Additionally, it sets `networkDefaults.evm.integrity`, `networkDefaults.evm.getLogsMaxAllowedRange`, `networkDefaults.evm.getLogsSplitOnError`, and `upstreamDefaults.evm.getLogsAutoSplittingRangeThreshold` — those are documented in their respective KB sections (`common/defaults.go:L100-L167`).

#### Cache policies (`CachePolicyConfig`)

| YAML path | Type | Default | Behavior / notes | Citation |
|-----------|------|---------|------------------|----------|
| `database.evmJsonRpcCache.policies[].network` | string | `"*"` | WildcardMatch against the network ID (e.g., `"evm:1"`, `"evm:*"`). Empty defaults to `"*"` via `SetDefaults`. | `common/config.go:L320`, `common/defaults.go:L608-L610`, `data/cache_policy.go:L67,L109` |
| `database.evmJsonRpcCache.policies[].method` | string | `"*"` | WildcardMatch against the JSON-RPC method. | `common/config.go:L321`, `common/defaults.go:L605-L607`, `data/cache_policy.go:L75,L117` |
| `database.evmJsonRpcCache.policies[].params` | []interface{} | `nil` (empty = matches any params) | Positional param matcher. Index `i` in config matches params[i] in the request. Map patterns recursively match all keys. Array patterns match by length then element. String values use `WildcardMatch`. Non-string values use `paramToString` equality. Missing params in request are treated as `nil`. | `common/config.go:L322`, `data/cache_policy.go:L166-L232` |
| `database.evmJsonRpcCache.policies[].finality` | DataFinalityState | `DataFinalityStateFinalized` (= 0, zero value) | Single exact-match finality state. For `MatchesForSet`: exact equality against response finality. For `MatchesForGet`: `finalized` request also matches `unfinalized` policies (to find data written before finalization); `unknown` finality matches any policy. | `common/config.go:L323`, `data/cache_policy.go:L99-L150` |

#### Rate limit rules (`RateLimitRuleConfig`)

| YAML path | Type | Default | Behavior / notes | Citation |
|-----------|------|---------|------------------|----------|
| `*.rateLimitBudgets[].rules[].method` | string | no default (required) | WildcardMatch against request method. Multiple rules can match the same method simultaneously — all matching rules are returned by `GetRulesByMethod`. | `common/config.go:L1811`, `upstream/ratelimiter_budget.go:L63-L78` |

#### Upstream method filters (`UpstreamConfig`)

| YAML path | Type | Default | Behavior / notes | Citation |
|-----------|------|---------|------------------|----------|
| `upstreams[].ignoreMethods` | []string | `nil` | WildcardMatch patterns: any matching method → upstream reports it unsupported (`ErrUpstreamMethodIgnored`). Applied before `allowMethods`. | `common/config.go:L731`, `upstream/upstream.go:L1375-L1385` |
| `upstreams[].allowMethods` | []string | `nil` | WildcardMatch patterns: any matching method → upstream treats it as supported, overriding `ignoreMethods`. | `common/config.go:L732`, `upstream/upstream.go:L1388-L1399` |

Same fields exist at `projects[].ignoreMethods`, `projects[].allowMethods` (project-level HTTP-server-enforced filters, `erpc/http_server.go:L499-L550`, `common/config.go:L520-L521`) and on `auth.strategies[].ignoreMethods`, `auth.strategies[].allowMethods` (auth strategy scope, `auth/authorizer.go:L83-L114`, `common/config.go:L2448-L2449`).

#### Aliasing (`AliasingRuleConfig`)

| YAML path | Type | Default | Behavior / notes | Citation |
|-----------|------|---------|------------------|----------|
| `server.aliasing.rules[].matchDomain` | string | — | WildcardMatch against the HTTP `Host` header (port stripped). First match wins. Supports all boolean operators. | `common/config.go:L258`, `erpc/http_server.go:L244-L257` |

#### CORS (`CORSConfig`)

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `projects[].cors.allowedOrigins` | []string | `["*"]` | Each entry matched via `WildcardMatch` against the `Origin` header; first match makes the origin allowed. | `erpc/http_server.go:L1024`, `common/defaults.go:L2824-L2827` |

#### Forward headers (`ProjectConfig`)

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `projects[].forwardHeaders` | []string | `nil` | WildcardMatch against each incoming request header name; matching headers are copied to upstream requests in `nq.ForwardHeaders`. | `common/config.go:L519`, `erpc/http_server.go:L480-L493` |

#### Upstream selector / `use-upstream` directive

The `use-upstream` directive value (set via `X-ERPC-Use-Upstream` header or query param) is matched against upstreams via `UpstreamMatchesSelector` — id first, then tags. Config fields involved:

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `upstreams[].id` | string | — | Primary identity matched against the selector pattern. | `common/matcher.go:L106-L112` |
| `upstreams[].tags` | []string | `nil` | Additional tag strings (format: `"key:value"` or bare `"value"`) matched against selector if no negation in pattern. | `common/config.go:L739 (approx)`, `common/matcher.go:L73-L100` |

#### Score multipliers (`ScoreMultiplierConfig`)

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `upstreams[].routing.scoreMultipliers[].network` | string | `""` (empty = any) | WildcardMatch against network ID; empty matches any. | `common/config.go:L798` |
| `upstreams[].routing.scoreMultipliers[].method` | string | `""` (empty = any) | WildcardMatch against method; empty matches any. | `common/config.go:L799` |
| `upstreams[].routing.scoreMultipliers[].finality` | []DataFinalityState | `[]` (any) | `slices.Contains` finality check. | `common/config.go:L800` |

#### Served-tip guaranteed methods

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `networks[].evm.servedTip.guaranteedMethods[]` | []string | — | Each element validated by `ValidatePattern` at config load. At runtime, WildcardMatch against the request method to determine if the served-tip mechanism applies. | `common/validation.go:L1323-L1326` |

#### Tracing force-trace matchers

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `tracing.forceTraceMatchers[].network` | string | `""` (absent = any) | WildcardMatch against network ID. If both network and method are set, both must match (AND). | `common/config.go:L241`, `common/tracing_core.go:L306-L329` |
| `tracing.forceTraceMatchers[].method` | string | `""` (absent = any) | WildcardMatch against method. | `common/config.go:L245`, `common/tracing_core.go:L324-L326` |

#### Provider network overrides

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `providers[].overrides` | map[string]UpstreamConfig | `nil` | Map keys are WildcardMatch patterns against the network ID. First matching key wins. | `thirdparty/provider.go:L80-L91` |

#### Static responses (`StaticResponseConfig`)

| YAML path | Type | Default | Behavior | Citation |
|-----------|------|---------|----------|----------|
| `networks[].staticResponses[].method` | string | required | Exact string equality (not WildcardMatch). | `common/static_response.go:L16` |
| `networks[].staticResponses[].params` | []interface{} | `nil` (nil config matches any params) | Deep equality with numeric-type tolerance (YAML int vs JSON float64). NOT WildcardMatch. | `common/static_response.go:L19-L39` |

#### Consensus quota tags

In `consensus/quota.go:L96` tag membership for quota matching uses `WildcardMatch(pattern, t)` against each tag string in the upstream config's `Tags` slice.

### DataFinalityState values

| YAML string | Go constant | iota value | Meaning |
|-------------|-------------|------------|---------|
| `"finalized"` | `DataFinalityStateFinalized` | 0 | Block number ≤ upstream's finalized block. Default/zero value — unset finality in config defaults to this. |
| `"unfinalized"` | `DataFinalityStateUnfinalized` | 1 | Block number known and > upstream's finalized block. |
| `"realtime"` | `DataFinalityStateRealtime` | 2 | Methods that return fresh, block-unbound data (e.g., `eth_gasPrice`). |
| `"unknown"` | `DataFinalityStateUnknown` | 3 | Block number cannot be determined (trace-by-hash calls, etc.). |
| (internal only) | `DataFinalityStateAll` | -1 | Internal wildcard for health-tracker cross-finality rollups. Never valid in config or on requests. |

YAML/JSON accepts string names or their numeric indices (`"0"` = finalized). Enum is defined at `common/data.go:L8-L76`.

### Behaviors & algorithms

**`WildcardMatch(pattern, value string) (bool, error)`** — full algorithm:

1. `tokenize(pattern)` (`common/matcher.go:L221-L272`): iterate bytes; `|` → `tokenOr`; `&` → `tokenAnd`; `!` → `tokenNot`; `(` → `tokenLParen`; `)` → `tokenRParen`; space → flush current atom (trimmed); any other byte → accumulate into current atom. Final accumulated atom flushed. No escaping mechanism exists for literal `|`, `&`, `!`, `(`, `)` characters.
2. `parseExpression()` → `parseOr()` → `parseAnd()` → `parseUnary()` → `parsePrimary()` — recursive descent, building closures.
3. `parsePrimary` for a pattern atom (`common/matcher.go:L362-L396`):
   - `pattern == "<empty>"` → return `value == ""`
   - `strings.TrimSpace(value)` applied first
   - If `strings.HasPrefix(value, "0x")`: check pattern for `>=`, `<=`, `>`, `<`, `=` prefixes in that order — call `compareNumbers(threshold, value, op)` on match
   - Otherwise (or if no numeric prefix): `wildcard.Match(pattern, value)` from the `go-wildcard` library
4. `compareNumbers` parses both strings with `parseNumber` (hex or decimal), performs the numeric comparison. If either parse fails, returns `false` silently.
5. `parser.current()` returns an empty-value token past end of token list so parsers never panic on EOF without panicking.
6. The returned closure is a `func(string) bool`; errors are only returned for tokenizer failures (currently tokenizer never errors) or parser failures (unmatched parens, missing operands).

**`ValidatePattern(pattern string) error`** — validation algorithm:

1. Empty string → `nil` (valid).
2. `recover()` deferred to catch panics and convert to `erpc_unexpected_panic_total{scope="validate-pattern"}`.
3. Tokenize, then sequentially validate: `tokenOr`/`tokenAnd` must have valid left (`tokenPattern`/`tokenRParen`) and right (`tokenPattern`/`tokenLParen`/`tokenNot`) neighbors and not be at position 0 or last. `tokenNot` must be followed by `tokenPattern` or `tokenLParen`. `tokenPattern` with numeric comparison prefix validates hex with `isValidHexNumber`.
4. Parens must be balanced (counter).
5. Full parse with `parseExpression` — if `p.pos < len(tokens)` after parse, returns "unexpected token" error.

Valid patterns accepted: `"*"`, `"eth_getLogs"`, `"eth_*"`, `"eth_get* | debug_*"`, `"(eth_* | debug_*) & !eth_call"`, `">=0x100 & <=0x200"`, `"!(latest|safe|finalized)"`, `"<empty>"`, `"family:systx"`.

Invalid patterns: `"| eth_call"` (OR at position 0), `"eth_call &"` (AND missing right operand), `"(eth_call"` (unclosed paren), `">0xZZZ"` (invalid hex), `"eth_call && debug_*"` (double operator), `"eth_call ! debug_*"` (NOT not at start).

**`SelectExecutor[E any]` — generic executor selection** (`common/match.go:L18-L78`):

```
for each executor e:
  methodMatch = (mPat=="" || mPat=="*") || matchMethodPattern(mPat, method)
  finalityMatch = len(fList)==0 || slices.Contains(fList, finality)
  if !methodMatch || !finalityMatch: skip
  bucket by: (specific-method, specific-finality) → (specific-method, any-finality) → (any-method, specific-finality) → (any-method, any-finality)
  take first in each bucket

return: bestMethodFinality ?? bestMethod ?? bestFinality ?? bestCatchAll
```

The search is a single pass over all executors — not 4 passes. Each tier's "first entry" is recorded. For a single-executor list the result is always that executor regardless of method/finality (assuming it passes the match test). Returns `(zero, false)` when nothing matches.

**Upstream `getFailsafeExecutor` — manual 4-pass implementation** (`upstream/upstream.go:L372-L408`):

Unlike `SelectExecutor`, this is 4 sequential linear scans over all executors, returning on first match in each tier. Behavior is identical to `SelectExecutor` but implemented explicitly. The 4 passes are: pass 1 = specific-method + specific-finality; pass 2 = specific-method + any-finality; pass 3 = any-method + specific-finality; pass 4 = any-method + any-finality.

**Network `getFailsafeExecutor` — config-order scan** (`erpc/networks.go:L910-L928`):

The network-level picker does NOT use 4-tier priority. It does a single linear scan in config order and returns the first entry where `methodMatches && finalityMatches`. This is subtly different from the 4-tier model: if an any-method entry appears before a specific-method entry in config, the any-method entry wins for specific methods. Operators must order failsafe entries from most-specific to least-specific for network scope.

**`CachePolicy.matchParams` algorithm** (`data/cache_policy.go:L166-L232`):

```
if len(config.Params) == 0: return true (any params)
for i, patternElem in config.Params:
  v = params[i] if i < len(params) else nil
  match = matchParam(patternElem, v)
  if !match: return false
return true
```

`matchParam(pattern, param)`:
- pattern is `map[string]interface{}` → param must also be a map; all keys in pattern must match recursively
- pattern is `[]interface{}` → param must be an array of same length; each element matched recursively
- pattern is `string` → `common.WildcardMatch(pattern, paramToString(param))`
- pattern is other type → `paramToString(pattern) == paramToString(param)`

`paramToString(x)`: nil→`""`, string→string, float64→`strconv.FormatFloat(v, 'f', -1, 64)`, bool→`strconv.FormatBool(v)`, other→`fmt.Sprintf("%v", v)`.

Key difference from static-response matching: static responses use exact equality, cache policies use `WildcardMatch` on string params. A cache policy with `params: ["latest"]` matches only requests with first param `"latest"`. A cache policy with `params: ["*"]` matches any first param (including numeric block tags).

**`MatchFinalities` — defaults-merge set-overlap** (`common/defaults.go:L17-L34`):

```
if len(f1)==0 || len(f2)==0: return true
for each f1 element: for each f2 element: if f1==f2: return true
return false
```

Used only in `SetDefaults` (not in request dispatch). Controls which `networkDefaults.failsafe[]` or `upstreamDefaults.failsafe[]` entry is copied as the base for a given upstream entry based on finality overlap.

**`MatchesSelector` tag matching guard** (`common/matcher.go:L73-L100`):

```
if pattern == "": return false
idMatch, err = WildcardMatch(pattern, id)
if idMatch: return true
if len(tags)==0 || strings.ContainsRune(pattern, '!'): return false
for each tag: if WildcardMatch(pattern, tag): return true
return false
```

The `'!'` check is a simple byte scan — any `!` anywhere in the pattern, even inside parentheses or in a negated sub-expression, disables tag matching. This is intentionally conservative.

**Auth strategy method filter semantics** (`auth/authorizer.go:L80-L115`):

```
shouldApply = true
for each ignoreMethod: if WildcardMatch(ignoreMethod, method): shouldApply=false; break
for each allowMethod:  if WildcardMatch(allowMethod, method):  shouldApply=true;  break
return shouldApply
```

Allow overrides ignore. To allow only `eth_getLogs`: set `ignoreMethods: ["*"]` and `allowMethods: ["eth_getLogs"]`. Same logic at upstream level (`upstream/upstream.go:L1375-L1399`).

**Rate-limit rule method matching** (`upstream/ratelimiter_budget.go:L63-L78`):

```
for each rule: if rule.Config.Method == method || WildcardMatch(rule.Config.Method, method): include in result
```

Note: `rule.Config.Method == method` check is redundant with `WildcardMatch` (the wildcard library handles exact strings). All matching rules are returned (not just the first), so multiple rules can apply to the same request simultaneously.

**Cache `MatchesForGet` — finality relaxation** (`data/cache_policy.go:L104-L149`):

- Request finality `finalized` → match policies with `finalized` OR `unfinalized` (data written as unfinalized may now be finalized).
- Request finality `unfinalized` → match only `unfinalized` policies.
- Request finality `realtime` → match only `realtime` policies.
- Request finality `unknown` → match any policy (iterate all; first stored result wins in caller).

**`MatchesForSet` — finality strict equality** (`data/cache_policy.go:L62-L102`):

On write, `p.config.Finality == finality` is exact equality — only the policy whose finality exactly matches the response's resolved finality is used for storage.

### Observability

**Prometheus metrics:**
- `erpc_unexpected_panic_total{scope="validate-pattern", extra="pattern:<pat>", error=<fingerprint>}` — fires when a tokenizer/parser panic occurs during `ValidatePattern` (`common/matcher.go:L121-L133`, defined `telemetry/metrics.go:L13`).
- `erpc_network_static_response_served_total{project, network, method}` — fired in `tryServeStaticResponse` on every static-response hit (`erpc/networks_static_responses.go:L55-L58`).

No dedicated matcher-specific metrics exist. Failsafe executor selection errors are not separately metered; errors in WildcardMatch calls on hot paths are silently ignored.

**Trace span attributes:**
- `failsafe.matched_method` and `failsafe.matched_finalities` are set as span attributes on the active failsafe span when a network-level failsafe executor is selected (`erpc/networks.go:L1179-L1180`).

**Notable log lines:**
- `"method support result"` at Debug with `allowed` bool and `method` string from upstream method support check (`upstream/upstream.go:L1402`).
- `"skipping static response: cannot inspect request"` at Debug when params cannot be read (`erpc/networks_static_responses.go:L23`).
- `"served static response (no upstream contacted)"` at Debug on hit (`erpc/networks_static_responses.go:L61`).
- `"error matching ignore method … with method …"` at Error in auth authorizer on WildcardMatch error (`auth/authorizer.go:L90`).

### Edge cases & gotchas

1. **No escaping for meta-characters.** `|`, `&`, `!`, `(`, `)` cannot appear as literal values inside a pattern. A method name like `net_!custom` is untestable. This is a hard constraint of the tokenizer (`common/matcher.go:L226-L258`).

2. **Spaces are atom separators, not literals.** `"eth_getLogs "` (trailing space) tokenizes to `["eth_getLogs"]` (trimmed). `"a | b"` and `"a|b"` are equivalent. Leading/trailing spaces on atom values are stripped via `strings.TrimSpace` (`common/matcher.go:L229,L235,L241,L246,L251,L260`). A pattern meant to match a method with a literal space would silently split into two tokens.

3. **Numeric comparison only fires on `0x`-prefixed values.** If the runtime value being tested is e.g. `"12345"` (decimal block number without `0x`), no numeric comparison occurs regardless of the pattern operator (`>`, `<`, etc.) — it falls through to glob match (`common/matcher.go:L373-L391`). The caller (cache params matching, etc.) must ensure values are hex-encoded to use numeric operators.

4. **Hex numeric overflow.** `parseNumber` uses `strconv.ParseInt(s, 16, 64)` which overflows for values > `0x7FFFFFFFFFFFFFFF`. Values beyond that (possible for large block numbers in other chains) parse silently as errors → `compareNumbers` returns `false` without error (`common/matcher.go:L399-L438`).

5. **No pattern compilation cache.** Every call to `WildcardMatch` tokenizes and builds closures from scratch. For very hot paths (failsafe selection on every request, rate-limit rule enumeration), this is re-done per call. Consider that `SelectExecutor` and `getFailsafeExecutor` are called at the start of every forwarded request.

6. **Network-level vs upstream-level executor selection differ in algorithm.** Upstream uses the 4-tier priority (`upstream/upstream.go:L372-L408`); network uses first-match in config order (`erpc/networks.go:L910-L928`). Same `matchMethod`/`matchFinality` fields, different selection semantics. To get consistent 4-tier behavior at network scope, users must manually order their `failsafe` entries from most-specific to least-specific.

7. **`MatchFinalities` empty-means-any only applies during defaults merging.** At request dispatch time (`slices.Contains(fl, finality)` in `SelectExecutor`), an empty `matchFinality` means catch-all via the explicit `len(fList)==0` check — not via `MatchFinalities`. They behave the same at runtime but `MatchFinalities` is a separate utility only called from `SetDefaults`.

8. **`MatchesSelector` negation guard is coarse.** `strings.ContainsRune(pattern, '!')` disables tag matching for any pattern with a `!` anywhere, including `"!other-upstream"` which would legitimately want tag-matching for positively included upstreams. This is by design: enabling tag matches for negated patterns would cause a non-matching tag to rescue an excluded id (`common/matcher.go:L87-L89`).

9. **Empty pattern in `MatchesSelector` always returns false.** Callers must guard `!= ""` before calling, per the function's doc comment (`common/matcher.go:L72`). Upstream selectors with empty `use-upstream` directives are excluded upstream-side by the directive default being empty string.

10. **Static response `method` is exact equality, not wildcard.** Unlike every other string matcher in the system, `FindStaticResponseMatch` uses `e.Method != method` (exact byte equality). A pattern like `"eth_getBlock*"` in `staticResponses[].method` will NOT match `eth_getBlockByNumber` (`common/static_response.go:L16`).

11. **`CachePolicyConfig.finality` zero-value is `finalized`.** An unset YAML finality key marshals to `0` = `finalized`. Operators who omit `finality` in a cache policy get a finalized-only policy without an error — this is the expected and documented default, but can surprise operators who expect the policy to match all finalities (`common/data.go:L15`).

12. **Concurrent `WildcardMatch` calls on same pattern are safe.** The engine creates new closure-trees per call (no shared mutable state). However, `RateLimiterBudget` holds a `rulesMu sync.RWMutex` protecting iteration over rules during `GetRulesByMethod`, and `Upstream.supportedMethods` (sync.Map) is written after method-support evaluation (`upstream/upstream.go:L1401`) — these are safe but note the latency cost of per-call tokenization under lock.

13. **`ValidatePattern` on empty string is a no-op returning nil** — callers that pass empty patterns to `WildcardMatch` are safe; the engine returns `wildcard.Match("", value)` which the `go-wildcard` library evaluates (empty glob matches only empty string, not all strings). But config fields often default empty patterns to `"*"` rather than leaving them empty (`common/defaults.go:L605-L610`).

14. **`paramToString` on float64 from JSON.** JSON numbers decode as `float64` in Go; `strconv.FormatFloat(v, 'f', -1, 64)` formats them without trailing zeros (`"1234"` not `"1234.000000"`). YAML integers decode as `int` and also reach `paramToString` as their Go type. The WildcardMatch call on `paramToString(param)` means a pattern `"123*"` would match JSON param `1234` (as `"1234"` string). Non-string params compared via string representation rather than deep equality (`data/cache_policy.go:L234-L249`).

15. **Multiple rate-limit rules can fire simultaneously for one request.** `GetRulesByMethod` returns ALL rules whose method pattern matches, not just the first. Each rule independently has a token-bucket counter; the request must acquire permits from all matching rules (`upstream/ratelimiter_budget.go:L63-L78`).

16. **`DataFinalityStateAll = -1` is an internal sentinel.** It is negative to not collide with the iota enum. It must never appear in config or be set on a request — it only lives in the health-tracker's cross-finality rollup keys (`common/data.go:L29-L36`).

17. **Pattern validation is not run on all config fields at startup.** `ValidatePattern` is currently called only for `network.*.evm.servedTip.guaranteedMethods` entries (`common/validation.go:L1323-L1326`). Other fields accepting patterns (cache policy method, failsafe matchMethod, etc.) are validated only implicitly — an invalid pattern produces a parse error the first time it is evaluated at runtime, silently skipped in most callers.

18. **Legacy single-object failsafe silently forces `matchMethod: "*"` even if the user explicitly set a non-`"*"` value in the old format.** The migration code at `common/config.go:L2094-L2100` (NetworkConfig), `L647-L652` (NetworkDefaults), and `L893-L897` (UpstreamConfig) only forces `"*"` when `MatchMethod == ""`. If the user wrote `matchMethod: "eth_call"` in the old single-object form, that value IS preserved — the `== ""` guard means explicit non-empty values pass through unchanged. This is tested in `common/config_test.go:L444-L459` ("NetworkDefaults old format with non-empty MatchMethod"). The wrapping into a 1-element slice still happens regardless of the `matchMethod` value.

19. **Legacy failsafe migration fires silently — no deprecation warning.** The `UnmarshalYAML` fallback path emits no log message or warning when it converts a single-object failsafe to an array. Operators migrating from old configs will not know the migration happened. The only indicator is at runtime: the in-memory `Failsafe` field is always a slice. (`common/config.go:L2041-L2103`, `L602-L656`, `L825-L905`).

20. **Zero-config baseline only applies when `projects:` is entirely absent or empty.** Even a `projects: []` (empty array) in YAML — or a project config with `id` only — will NOT trigger the auto-generated 'main' project. The condition is `len(c.Projects) == 0` at `common/defaults.go:L100`. If the user defines even one empty project, these specific failsafe defaults do NOT exist, and upstreams will have no failsafe unless the user or `upstreamDefaults` provides one.

21. **Cache policy `MatchesForGet` returns `true` for `unknown` finality.** When the request finality cannot be determined, the getter iterates ALL policies (network+method matched) to check if any stored result exists. The caller `data/cache_executor.go` picks the first connector that returns data. This means a request with unknown finality can be served from a `finalized` policy's cache, potentially returning stale data for some edge cases (`data/cache_policy.go:L146-L149`).

### Source map

- `common/matcher.go` — `WildcardMatch`, `MatchesSelector`, `UpstreamMatchesSelector`, `ValidatePattern`, tokenizer, recursive-descent parser, `compareNumbers`, `parseNumber`.
- `common/match.go` — `SelectExecutor[E]` generic 4-tier executor picker, `matchMethodPattern` helper.
- `common/static_response.go` — `FindStaticResponseMatch`, `paramsEqual`, `valueEqual`, `toFloat64` for static-response exact-equality matching (NOT WildcardMatch).
- `common/data.go` — `DataFinalityState` enum, string marshaling, `DataFinalityStateAll` sentinel.
- `common/defaults.go` — `MatchFinalities` (set-overlap for finality in defaults inheritance), `CachePolicyConfig.SetDefaults` (defaults `method="*"`, `network="*"`), `FailsafeConfig.SetDefaults` (defaults `matchMethod="*"`), `Config.SetDefaults` zero-config baseline at `L100-L167` (auto-generates 'main' project with pre-filled networkDefaults and upstreamDefaults failsafe entries when no projects are configured).
- `common/validation.go` — `FailsafeConfig.Validate` (rejects empty matchMethod), `ValidatePattern` invocation for `evm.servedTip.guaranteedMethods`.
- `common/config.go` — struct definitions: `FailsafeConfig` (matchMethod, matchFinality), `CachePolicyConfig` (network, method, params, finality), `RateLimitRuleConfig` (method), `AliasingRuleConfig` (matchDomain), `ForceTraceMatcher` (network, method), `ScoreMultiplierConfig` (network, method, finality), `StaticResponseConfig` (method, params). Also contains three `UnmarshalYAML` implementations for backward-compat single-object failsafe → array migration: `NetworkConfig.UnmarshalYAML` (`L2041-L2103`), `NetworkDefaults.UnmarshalYAML` (`L602-L656`), `UpstreamConfig.UnmarshalYAML` (`L825-L905`).
- `data/cache_policy.go` — `CachePolicy.MatchesForSet`, `MatchesForGet`, `matchParams`, `matchParam`, `paramToString` — the cache param-matching engine.
- `erpc/networks.go` — network-level `getFailsafeExecutor` (config-order linear scan), `requestSelector`/`filterBySelector` for `use-upstream`, served-tip partition grouping via `UpstreamMatchesSelector`.
- `erpc/networks_static_responses.go` — `tryServeStaticResponse` coordinator.
- `upstream/upstream.go` — upstream `getFailsafeExecutor` (4-tier manual loop), `isAllowedMethod` (ignoreMethods/allowMethods), `use-upstream` directive enforcement per-upstream via `UpstreamMatchesSelector`.
- `upstream/ratelimiter_budget.go` — `GetRulesByMethod` (WildcardMatch on rule method).
- `auth/authorizer.go` — `shouldApplyToMethod` (ignoreMethods before allowMethods).
- `erpc/http_server.go` — project-level ignoreMethods/allowMethods, forwardHeaders, CORS origin, aliasing matchDomain — all via WildcardMatch.
- `common/tracing_core.go` — `matchesForceTraceMatcher` (WildcardMatch on network AND method with AND semantics).
- `consensus/quota.go` — WildcardMatch on tags for quota matching.
- `thirdparty/provider.go` — WildcardMatch on provider overrides map keys (network patterns).
- Tests: `common/matcher_test.go` (WildcardMatch, MatchesSelector, ValidatePattern — comprehensive table-driven), `common/static_response_test.go` (if present; paramsEqual edge cases).
