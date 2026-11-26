# AGENTS.md — eRPC

Project rules for every AI coding agent (Codex, Cursor, Claude Code, Maestro).
`CLAUDE.md` imports this file; there is no other rules file.

## Project

- **eRPC** — fault-tolerant EVM RPC proxy with re-org-aware permanent caching.
- Go server (`cmd/erpc`) + TypeScript packages under `typescript/` (`@erpc-cloud/config`, `@erpc-cloud/cli`).
- Module: `github.com/erpc/erpc`. Docs: <https://docs.erpc.cloud/>.

## Design Razor: the Weakest Hypothesis, Not the Shortest

Source: M. T. Bennett, *The Optimal Choice of Hypothesis Is the Weakest, Not
the Shortest* (AGI-23; [arXiv:2301.12987](https://arxiv.org/abs/2301.12987)).
The result in one line: among all hypotheses that EXACTLY account for what has
been observed, the one most likely to also hold for unseen cases is the
**weakest** — the one with the largest *extension* (the set of possibilities it
still admits / inputs it still handles correctly). The paper proves weakness is
necessary and sufficient to maximise the probability of generalising, while
shortness/simplicity is neither. Its one-line razor: **"Explanations should be
no more specific than necessary."**

Generality is a property of a design's EXTENSION, not its FORM. "All things are
blue crabs" is short and maximally overcommitted; a verbose pass-through can
commit to almost nothing. A tidy regex or a clean enum is not evidence of
generality.

This razor is unusually load-bearing in eRPC because nearly every dimension we
operate on is an open-ended set: chains, vendors, JSON-RPC methods, error
shapes, response quirks, client behaviours. New members arrive weekly and
UNANNOUNCED — the cost of an overcommitment here is silent misbehaviour on live
traffic, not a compile error.

Concretely, for every design and review in this repo:

1. **The fallthrough is the primary path.** For any open set (methods, vendor
   errors, response shapes, chains), much of tomorrow's traffic through your
   code will be the case you did NOT enumerate. Design and test the
   unknown-case default first; enumerated cases are optimisations layered on
   top. A method table, error matcher, or chain special-case is acceptable only
   when the unmatched path is safe, correct, and observable.
2. **No unforced commitments.** Hard-coded `eth_*` method lists, vendor
   error-string/regex matching, chain-ID special cases in core packages,
   thresholds derived from "all chains/vendors we checked behave this way" —
   each is a claim about ALL future members of an open set. If today's observed
   data doesn't force the claim, don't make it.
3. **Weaken by deleting structure, never by adding speculation.** Prefer
   string + discovery over enum; kind + metadata over N parallel
   structs/tables/endpoints; verbatim pass-through over interpretation; config
   over code constants. Speculative abstraction (layers, hooks, options for
   imagined futures) is not weakening — unexercised machinery is itself a
   commitment. YAGNI governs form; the razor governs claims.
4. **Weak ≠ vague.** The design must still decide every observed case exactly
   (verified against real fixtures and tests), and open-ended inputs must still
   resolve INTO a bounded, low-cardinality interface: any vendor error → one of
   the finite normalized error codes; any method → one of the finite
   cache/routing behaviours. Weakness in the claims, boundedness at the
   interface.
5. **Known invariants are data, not overreach.** True wire/protocol facts
   (JSON-RPC 2.0 framing, hex-quantity encoding, chainId immutability) are
   encoded as explicit, validated commitments at the edge — never smeared as
   incidental assumptions through core logic. But measured behaviour is not an
   invariant: N vendors or chains agreeing is an observation, not a law (a
   traced-gas bound measured across six chains was later violated by EIP-3529
   refunds on a seventh; the fix was deleting the bound, not adding an
   exception). When reality violates a "bound", delete it — don't stack
   exceptions.
6. **No knobs for never-right behaviour.** A config flag whose one setting no
   real use-case wants is added form with negative extension — fix the
   semantics and delete the knob instead of shipping an opt-out for wrongness.
7. **Review test.** For every design/PR ask: *what unseen-but-plausible input
   does this silently mishandle, and what in today's observed data forces that
   commitment?* If nothing forces it, weaken the design.

## Bootstrap commands

```bash
make setup           # go mod tidy
pnpm install --ignore-scripts   # JS workspace (skip postinstall, matches CI)

make fmt             # format Go (also Biome for TS via pnpm)
make build           # build the eRPC server
make test-fast       # parallel-sharded unit tests (daily-driver)
make test            # full suite with -race (slow, run before merge / CI runs this)
make test-race       # race detector on a slimmer scope
pnpm -r run build    # build the TypeScript packages
```

Single test: `LOG_LEVEL=trace go test -run <pattern> ./...`

## Testing Guidelines

### Test Initialization
- **Always** include this in test files that need logging:
  ```go
  func init() {
      util.ConfigureTestLogger()
  }
  ```
- Use the global logger: `log.Logger` from `github.com/rs/zerolog/log`
- For debugging tests, use: `LOG_LEVEL=debug go test -run <pattern> ./...` or `LOG_LEVEL=trace` for even more detail

### Gock (HTTP Mocking) Best Practices
1. **Setup Order is Critical**: Always set up ALL gock mocks BEFORE initializing any network components or services
   ```go
   // ✅ CORRECT: Set up mocks first
   util.ResetGock()
   defer util.ResetGock()
   util.SetupMocksForEvmStatePoller()
   
   // Set up your test-specific mocks here
   gock.New("http://rpc1.localhost")...
   
   // THEN initialize network/services
   network := setupTestNetwork(t, ctx, ...)
   ```

2. **Standard Gock Pattern**:
   ```go
   util.ResetGock()
   defer util.ResetGock()
   util.SetupMocksForEvmStatePoller()  // Handles background state polling
   defer util.AssertNoPendingMocks(t, 0)  // 0 or expected pending count
   ```

3. **Filtering Requests**: Use filters to distinguish between different request types
   ```go
   gock.New("http://rpc1.localhost").
       Post("").
       Filter(func(r *http.Request) bool {
           body := util.SafeReadBody(r)
           return strings.Contains(body, "eth_getBalance")
       }).
       Reply(200).
       JSON(map[string]interface{}{...})
   ```

4. **Mock Persistence**:
   - Use `Times(1)` for mocks that should only match once
   - Use `Persist()` for mocks that might be called multiple times or in unpredictable patterns
   - For concurrent tests or tests with background processes, use `Persist()`

### Context Management
- Always create and properly clean up contexts:
  ```go
  ctx, cancel := context.WithCancel(context.Background())
  defer cancel()
  ```
- Pass this context through all network/service operations

### Race Condition Prevention
1. **Never use `t.Parallel()`** with tests that use gock mocks
2. Set up all mocks before component initialization (components may start background goroutines)
3. Be aware that network initialization starts background processes like state pollers
4. For concurrent tests, don't use `defer util.AssertNoPendingMocks(t, 0)`

### Common Debugging Tips
- If tests fail with "gock: cannot match any request", check:
  1. Are mocks set up BEFORE component initialization?
  2. Are filters too restrictive?
  3. Are background processes consuming mocks?
- Run with race detector during development: `go test -race -run TestName ./...`
- Use `gock.Clean()` sparingly - prefer proper mock setup order

## Advanced Testing Patterns

### Mock Creation Best Practices
- When working with gock for different response bodies, create separate simple mocks:
  ```go
  // ✅ GOOD: Simple, readable mocks
  gock.New(url).Post("").Times(1).Reply(200).JSON(response1)
  gock.New(url).Post("").Times(1).Reply(200).JSON(response2)
  
  // ❌ AVOID: Complex body generation functions
  ```

### Subscription and Integration Tests
- Always set required configuration values that might have zero defaults:
  - `MaxSyncRangeSize` (e.g., 1000)
  - `HttpParallelBlockFetches` (e.g., 10)
- Ensure parent hash chains are properly connected in blockchain tests:
  ```go
  if blockNum == 1 {
      parentHash = []byte("genesis")  // Block 1 points to genesis
  } else {
      parentHash = []byte(fmt.Sprintf("hash-%d", blockNum-1))
  }
  ```
- Use `SetID()` method on JsonRpcResponse objects to set the ID field correctly

### Test Execution Patterns
- For debugging specific tests: `LOG_LEVEL=trace go test -run TestName ./... -v`
- When tests timeout, check for:
  - Parent hash mismatches in blockchain data
  - Missing required configuration values
  - Improper mock setup order

## Error Handling Patterns

### Error Type Checking
- Use `common.HasErrorCode(err, codes...)` to check for specific error codes in the error chain
- For extracting specific error types from nested errors, use `errors.As()`:
  ```go
  var jre *common.ErrJsonRpcExceptionInternal
  if errors.As(err, &jre) {
      normalizedCode := jre.NormalizedCode()
      // Use the extracted error
  }
  ```

### JSON-RPC Error Handling
- `ErrJsonRpcExceptionInternal` contains normalized codes that should be used for comparison
- Access normalized code with `err.NormalizedCode()` method
- Common normalized codes:
  - `JsonRpcErrorEvmReverted` (3) - EVM execution reverted
  - `JsonRpcErrorCallException` (-32000) - Call exception
  - `JsonRpcErrorTransactionRejected` (-32003) - Transaction rejected

### Consensus Error Patterns
- Execution exceptions (like smart contract reverts) are valid consensus results
- When comparing errors for consensus, compare by their normalized JSON-RPC codes
- Exception: missing-data errors are compared by verdict, not by code. A provider's
  number for "I do not have this" describes its own storage layout, not the chain
  (agave: `-32007` local blockstore vs `-32009` long-term storage; `-32001` pruned
  ledger vs `-32004` unrooted slot), so comparing it manufactures disputes
- Use `common.ErrCodeEndpointExecutionException` to identify execution errors

## Consensus Implementation

### Treating Errors as Valid Results
- Execution exceptions represent valid blockchain state (e.g., all nodes agree a transaction reverts)
- Implement `isConsensusValidError()` to identify which errors contribute to consensus
- Error consensus should compare normalized codes, not just error types

### Error Hash Generation
- For consensus comparison, generate hashes that include the normalized error code,
  except for missing-data, which hashes by verdict plus permanence:
  ```go
  if common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
      hash = "missingdata"
  } else if errors.As(err, &jre) {
      hash = fmt.Sprintf("jsonrpc:%d", jre.NormalizedCode())
  }
  if common.IsPermanentlyMissingData(err) {
      hash += ":permanent"
  }
  ```
- The hash is never what the client sees; `buildErrorResponseBody` writes
  `NormalizedCode()`, which stays the upstream's native number on SVM

### Short-Circuit Logic
- Consensus can be reached on errors just like successful results
- When implementing short-circuit logic, consider both successful responses and consensus-valid errors
- Count participants with consensus-valid errors as valid participants

## Code Patterns and Best Practices

### Working with Upstreams
- Always check if upstream and its config are not nil before accessing:
  ```go
  if r.upstream != nil && r.upstream.Config() != nil {
      upstreamId := r.upstream.Config().Id
  }
  ```

### Goroutine Management in Tests
- Be careful with goroutine leaks in tests
- Use proper context cancellation to clean up goroutines
- When collecting responses from multiple goroutines, always handle nil responses from cancelled goroutines

## Go Concurrency Patterns

### Grab Under Lock, Use Outside
When reading shared fields that another goroutine may write, copy them to locals under `RLock`, then use the locals after releasing:
```go
e.stateMu.RLock()
cfg := e.cfg
label := e.networkLabel
e.stateMu.RUnlock()

// use cfg and label freely here
```
Never hold the lock while doing slow work (RPC calls, I/O, channel sends).

### Atomics for Hot-Path Counters
Use `atomic.Int64` / `atomic.Uint64` for single values read and written from multiple goroutines. Prefer atomics over a mutex when you only need a single load/store — they're cheaper and can't deadlock.

### Config Setters and Consumers Run on Different Goroutines
Methods like `SetNetworkConfig` are called from the project registry goroutine, while the ticker goroutine and request-serving goroutines read that config. Any field shared between them must be protected by a mutex or atomic. When adding new shared state, check who writes it and who reads it.

## Observability runbooks

Metric-analysis guides for the Grafana dashboards (`monitoring/grafana/`) — read
the relevant one before reasoning about a panel:

- [`monitoring/catch-up-metrics.md`](monitoring/catch-up-metrics.md) — how to
  read the **Catch-up Retries / Wait / Pressure** panels (data-not-yet-available
  retries). A non-zero baseline is normal; judge by pressure trend + per-chain
  wait sanity, not raw counts.

## PR expectations

- One logical change per PR.
- `make fmt` clean.
- `make test-fast` green locally; `make test` (race) green in CI.
- Brief test plan in the description.
- **Docs ride along:** any new feature, config field, changed default, new
  edge case, or new metric updates the matching page under `docs/pages/` in
  the same PR (see "Docs conventions" below).
- [Conventional Commits](https://www.conventionalcommits.org/) for commit messages (`feat:`, `fix:`).
- Feature branch per change; PRs target `main`.

## Docs conventions (when your change touches behavior)

The docs are agent-first: each page is a short promise-level visible body +
an exhaustive `<AISection>` agent panel. Full conventions live in
[`CONTRIBUTING.md`](CONTRIBUTING.md#documentation); the agent-critical rules:

- Put implementation detail ONLY in the page's `<AISection>` panel — update
  its **Config schema** table (exact default, cited to
  `common/defaults.go` as a GitHub permalink), **Edge cases & gotchas**
  (numbered, grounded in code or a test), and **Observability** (exact metric
  names + labels) sections. Touch the visible body only if the user-facing
  promise changed.
- Verify defaults/behavior against code before writing — never from memory.
  Match the exemplar page `docs/pages/config/failsafe/hedge.mdx`.
- Quick-taste snippets: full parent chain from the config root, comments on
  their own line above the value, `focusYaml`/`focusTs` highlighting only the
  teaching lines.
- Keep `<PromptExample>` blocks goal-phrased and config-format agnostic
  ("my eRPC config"); reference URL is the page's own
  `https://docs.erpc.cloud/<path>.llms.txt`.
- Never include operator-identifiable details in examples (internal
  hostnames, cluster keys, company/platform fingerprints) — neutral
  placeholders only.
- `.llms.txt` files are generated by `docs/scripts/build-llms.mjs` at build
  time — never hand-edit; new pages need a `_meta.js` entry to enter the nav
  and the llms index.

## Public Repository Content

- Keep branches, commits, PR titles, PR bodies, and other public text self-contained.
- Never include private issue identifiers, private workspace URLs, or assume readers can access internal systems.

## Additional Notes
- All contributions fall under the [Contributor License Agreement](CLA.md) and require adherence to the [Code of Conduct](CODE_OF_CONDUCT.md).
- Documentation lives under the `docs/` directory. See `README.md` for a project overview and quick start instructions.
