# KB: TypeScript config package & dist examples

> status: complete
> source-dirs: typescript/config, typescript/cli, erpc.dist.ts, erpc.dist.yaml, tygo.yaml, package.json, pnpm-workspace.yaml, common/config.go (TS loader), common/compiler.go, common/runtime.go, common/console.go, common/duration.go, common/data.go, cmd/erpc/main.go, internal/policy/runtime_pool.go, internal/policy/dump.go, internal/policy/eval.go, .github/workflows/release.yml, Dockerfile

## L1 — Capability (CTO view)

eRPC configs can be written in TypeScript (`erpc.ts`) instead of YAML, with full IDE autocompletion and compile-time type-checking provided by the npm package `@erpc-cloud/config` (`typescript/config/package.json:L2`). The TS type surface is machine-generated from the Go config structs via [tygo](https://github.com/gzuidhof/tygo) (`tygo.yaml:L1-L44`), so the npm package tracks the Go schema release-for-release. The same Go binary loads `.ts`/`.js` files directly — esbuild is embedded in-process as a Go library and the result runs in a sobek (pure-Go) JS engine, so **no Node.js is required at runtime** (`common/compiler.go:L10-L41`, `common/config.go:L2686-L2766`). The repo ships two canonical example configs (`erpc.dist.ts`, `erpc.dist.yaml`) and a thin npm CLI (`@erpc-cloud/cli`, also published as `start-rpc` and `start-erpc`) that downloads the matching platform binary at `npm install` time (`.github/workflows/release.yml:L207-L243`, `typescript/cli/src/install.ts`).

The unique payoff of the TS path: `selectionPolicy.evalFunc` can be a **real arrow function** (closures, imports, module-level helpers all preserved), not a JS source string — the loader never stringifies the function (`common/config.go:L2605-L2613`).

## L2 — Mechanics (staff-engineer view)

### Workspace layout

pnpm workspace with `packages: ['typescript/*']` (`pnpm-workspace.yaml:L1-L2`). Two packages:

- **`@erpc-cloud/config`** (`typescript/config/`) — pure type library + one runtime function (`createConfig`). Composed of: `src/generated.ts` (tygo output, 1713 lines, 226 exported symbols, "DO NOT EDIT" — `typescript/config/src/generated.ts:L1`), hand-written type refinements `src/types/generic.ts` + `src/types/policyEval.ts`, selection-policy scope/finality constants `src/constants.ts`, and the public barrel `src/index.ts`. Built output `lib/` (CJS bundle + `.d.ts`) is **checked into git** (verified via `git ls-files typescript/`).
- **`@erpc-cloud/cli`** (`typescript/cli/`) — binary-download wrapper. `bin.ts` spawns the downloaded native binary forwarding all argv (`typescript/cli/src/bin.ts:L6-L10`); `install.ts` runs on `postinstall` and fetches `erpc_<platform>` from the GitHub release pinned in `src/generated/release.ts` (`typescript/cli/src/install.ts:L68-L108`, `typescript/cli/package.json:L19`).

The repo **root** `package.json` is also versioned (`"version": "0.0.64"`, `package.json:L18`) and depends on `"@erpc-cloud/config": "workspace:*"` (`package.json:L11`), which materializes `node_modules/@erpc-cloud/config -> ../../typescript/config` as a symlink — this same trick is what makes imports resolve inside the Docker image (below).

### Type generation (tygo)

`tygo.yaml` generates TS from exactly one Go package: `github.com/erpc/erpc/common` → `typescript/config/src/generated.ts`, with `flavor: "yaml"` (property names come from `yaml:` struct tags) (`tygo.yaml:L2-L4`). 14 non-config files in `common/` are excluded (`utils.go`, `thirdparty.go`, `errors.go`, `json_rpc.go`, `request.go`, `response.go`, `duration.go`, `defaults.go`, `matcher.go`, `runtime.go`, `upstream_fake.go`, `vendors.go`, `tracing_core.go`, `tracing_util.go` — `tygo.yaml:L30-L44`). Generation is invoked as `go install github.com/gzuidhof/tygo@latest && tygo generate` during release preparation (`.github/workflows/release.yml:L44-L47`) — **not** in the Makefile, not on every commit; the generated file is committed.

Go structs steer the TS output through `tstype:` struct tags (75 occurrences in `common/*.go`; census: 45× `Duration`, 4× `Duration | AdaptiveDuration`, 2× `ByteSize`, 2× `-` (field omitted from TS), plus one-offs like `SelectionPolicyEvalFunction | string`, `TsConnectorConfig[]`, `RateLimitPeriod`, literal unions like `'file' | 'env' | 'secret'`). Because tygo cannot remap bare identifiers (only `ast.SelectorExpr`; blocked on tygo PR #74 — documented in `tygo.yaml:L6-L13`), the `frontmatter` block injects imports of hand-written types under `Ts*` aliases (`TsConnectorConfig`, `TsUpstreamType`, `TsAuthStrategyConfig`, …) which the `tstype` tags then reference (`tygo.yaml:L15-L29`, used at `typescript/config/src/generated.ts:L302,L354,L498,L521,L1021,L1323,L1342,L1348`).

### Go-side `.ts` loading pipeline

`common.LoadConfig` dispatches on filename suffix: `.ts`/`.js` → `loadConfigFromTypescript`, everything else → `os.ExpandEnv` + strict YAML decode (`common/config.go:L87-L132`). **Critical difference**: `os.ExpandEnv` (shell-style `${VAR}` interpolation on raw file bytes) is only applied to the YAML branch; the TS/JS branch never calls it. TS configs access environment variables at JS runtime via `process.env.X` (populated from `os.Environ()` in `common/runtime.go:L23-L36`) or via the `env` global (raw `os.Environ()` string array). The TS pipeline (`common/config.go:L2686-L2766`):

1. **Bundle**: esbuild Go API, `Bundle: true`, `Format: IIFE`, `GlobalName: "exports"`, `Target: ES2020`, `Platform: Node`, inline sourcemap; loaders for `.ts`/`.js`/`.json` (`common/compiler.go:L10-L41`). Imports (including `@erpc-cloud/config` itself) are bundled in — `createConfig` is an identity function, so it costs nothing (`typescript/config/src/index.ts:L164-L168`).
2. **Append walker**: `tsLoaderWalker` JS is appended to the bundle. After the user code runs, it walks the default export's object tree depth-first, assigns each function-valued object property a sequential id `fn_<n>`, registers it on `globalThis.__erpcFns[id]`, and stamps the function with a non-enumerable `__erpcFnId` (`common/config.go:L2615-L2658`). The walk is order-deterministic so every runtime that later re-runs the same program assigns identical ids (`common/config.go:L2627-L2630`).
3. **Compile once** with `sobek.Compile` → `*sobek.Program` (`common/config.go:L2697-L2701`).
4. **Run in a throwaway runtime** (`common.NewRuntime`: sets `env` array, `process.env` map from `os.Environ()`, and a `console` bridged to zerolog — `common/runtime.go:L15-L44`, `common/console.go:L13-L40`). The default export must exist or load fails with `"config object must be default exported from TypeScript code AND must be the last statement in the file"` (`common/config.go:L2714-L2717`).
5. **JSON.stringify with replacer**: function values with `__erpcFnId` become the sentinel string `"__ts_fn__:fn_<n>"`; functions the walker didn't catch are dropped (`return undefined`) — never `.toString()`-serialized (`common/config.go:L2730-L2742`).
6. **Strict YAML decode of the JSON** (JSON ⊂ YAML): `decoder.KnownFields(true)`, so typos error exactly like YAML configs, and all `UnmarshalYAML` hooks (legacy `group:`→`tier:` tag migration, finality string/int coercion, etc.) fire identically (`common/config.go:L2751-L2759`; proven by `common/config_test.go:L147-L252`).
7. **Attach** the compiled program as `cfg.UserScript` (`common/config.go:L2761-L2764`, field at `common/config.go:L51-L67`).

After load, the shared path applies `LegacyTranslateFn`, `SetDefaults`, `Validate` regardless of source format (`common/config.go:L111-L129`).

### Function-preservation at policy runtime

Each policy-engine runtime-pool acquire re-runs `cfg.UserScript` (which ends with the walker), rebuilding `__erpcFns` natively in that runtime, then installs shared helpers (`internal/policy/runtime_pool.go:L41-L82`). At tick time, an `EvalFunc` beginning with `__ts_fn__:` (checked via `common.IsTSFunctionSentinel`, `common/config.go:L2771-L2779`) is resolved by id lookup in `__erpcFns` and invoked directly (`internal/policy/eval.go:L587-L621`). Because the whole module is evaluated in each runtime, module-level helpers and closures referenced by the function exist there — the regression test calls a function whose body reads a module-level `weights` constant and gets the right value (`common/config_test.go:L262-L348`). `erpc dump` best-effort resolves sentinels back to JS source via the function's `.toString()` in a throwaway runtime, leaving the sentinel in place on any error (`internal/policy/dump.go:L32-L136`, `cmd/erpc/main.go:L144-L198`).

### Distribution & version sync

One version string rules everything: release workflow bumps `npm version <tag>` in `.`, `typescript/cli`, `typescript/config` (`.github/workflows/release.yml:L84-L91`), regenerates tygo types (`L44-L47`), runs goreleaser snapshot to compute binary checksums, then generates `typescript/cli/src/generated/{release,checksums}.ts` from `dist/checksums.txt` (`L93-L101`, generator: `typescript/cli/src/script/generate-release-files.ts`). All of that lands in a `chore: release <ver>` PR; merging it triggers the publish job: git tag + goreleaser real release, then `pnpm publish` of `@erpc-cloud/cli` (plus sed-renamed `start-rpc` and `start-erpc` aliases) and `@erpc-cloud/config` (`L126-L251`). The npm CLI therefore downloads exactly the binary version equal to its own package version (`typescript/cli/src/install.ts:L73`, `typescript/cli/src/generated/release.ts:L5-L6` — currently `0.0.64` / commit `eac201e5…`). The Go binary's own version is injected via ldflags `-X github.com/erpc/erpc/common.ErpcVersion` (`Dockerfile:L30`, `common/config.go:L25-L31`, defaults `"dev"`/`"none"`).

**Docker**: the final distroless image carries `/typescript` (built `@erpc-cloud/config` from the `ts-dev` stage) and `/node_modules` (prod install from `ts-prod`, containing the `@erpc-cloud/config -> ../typescript/config` symlink) alongside the Go binary (`Dockerfile:L36-L87`). That is why a user can `docker run -v $(pwd)/erpc.ts:/erpc.ts …` and `import { createConfig } from "@erpc-cloud/config"` resolves — esbuild walks up from `/erpc.ts` into `/node_modules`. Any *other* npm dependency must be provided by the user (custom image layering `node_modules`, or `-v` mounts), per `typescript/config/README.md:L59-L98`.

### Failure modes

- esbuild build errors surface as `"build failed: <messages>"` (`common/compiler.go:L31-L33`); compile errors as `"compile ts config: …"` (`common/config.go:L2700`); decode typos as `"ts config decode: …"` with the same field-name errors YAML gives (`common/config.go:L2758`).
- Missing default export → explicit error (see step 4 above).
- A TS-defined `evalFunc` whose runtime registry is missing produces `"ts selectionPolicy.evalFunc lookup: __erpcFns registry not populated (user script did not run?)"` / `"id %q not found in __erpcFns"` (`internal/policy/eval.go:L606-L621`).
- CLI binary download failures: ENOENT at spawn → `"erpc binary not found. Try reinstalling @erpc-cloud/cli"` (`typescript/cli/src/bin.ts:L12-L19`); 404 at install → `"Binary not found. Please check if the version exists in the releases."` (`typescript/cli/src/install.ts:L92-L107`).

## L3 — Exhaustive reference (agent view)

### Config fields

This subsystem owns no YAML runtime keys; its "config surface" is (a) the npm packages' manifests, (b) `tygo.yaml`, (c) the hand-written TS type aliases and exported constants, and (d) the loader's environment contract. Every row cites source.

#### `typescript/config/package.json` (@erpc-cloud/config)

| field | value | notes | citation |
|---|---|---|---|
| `name` | `@erpc-cloud/config` | npm package id | `typescript/config/package.json:L2` |
| `version` | `0.0.64` | bumped in lockstep with root + cli by release workflow | `typescript/config/package.json:L3`, `.github/workflows/release.yml:L84-L91` |
| `main` | `index.js` | **dangling** — no such file exists at package root (real bundle is `lib/index.js`); modern resolvers use `exports` instead | `typescript/config/package.json:L9` |
| `types`/`typings` | `./lib/index.d.ts` | | `typescript/config/package.json:L10-L11` |
| `exports["."]` | types→`./lib/index.d.ts`, require/import/default→`./lib/index.js` | CJS served for both `require` and `import` | `typescript/config/package.json:L23-L30` |
| `files` | `lib`, `src/*.ts`, `src/types/*.ts` | ships sources so go-to-definition lands on annotated TS | `typescript/config/package.json:L18-L22` |
| `scripts.build` | `build:lib` (esbuild CJS bundle → `lib/`, with sourcemap) + `build:types` (tsc `--emitDeclarationOnly --declaration --declarationMap`, module nodenext) | | `typescript/config/package.json:L13-L15` |
| `scripts.test` | `tsc -p tsconfig.test.json` | compile-only (`noEmit: true`) type tests incl. `src/__tests__/erpc.test-d.ts` | `typescript/config/package.json:L16`, `typescript/config/tsconfig.test.json:L1-L9` |
| `license` | Apache-2.0 | | `typescript/config/package.json:L35` |

tsconfig (build): `outDir lib`, `declaration`, `strict`, `module CommonJS`, `target ES6`, `moduleResolution Node`, `include src/*` (`typescript/config/tsconfig.json:L1-L18`).

#### `typescript/cli/package.json` (@erpc-cloud/cli)

| field | value | notes | citation |
|---|---|---|---|
| `name` | `@erpc-cloud/cli` | re-published under aliases `start-rpc`, `start-erpc` via sed at release time | `typescript/cli/package.json:L2`, `.github/workflows/release.yml:L215-L243` |
| `version` | `0.0.64` | must equal the GitHub release tag the installer downloads | `typescript/cli/package.json:L3`, `typescript/cli/src/install.ts:L73` |
| `bin` | `./dist/bin.js` | spawns native binary with `stdio: "inherit"`, forwards `process.argv.slice(2)`, exits with child's code | `typescript/cli/package.json:L5`, `typescript/cli/src/bin.ts:L6-L23` |
| `scripts.postinstall` | `node dist/install.js` | downloads `erpc_<platform>` from `https://github.com/erpc/erpc/releases/download/<version>/<binaryName>` into `dist/bin/`, `chmod 0o755`; **skipped when `process.env.CI` is set** | `typescript/cli/package.json:L19`, `typescript/cli/src/install.ts:L68-L113` |
| `scripts.prepare` | `npm run build` (tsc) | | `typescript/cli/package.json:L18` |
| `scripts.preuninstall` | `rm -rf bin` | | `typescript/cli/package.json:L20` |
| `files` | `dist`, `src`, `tsconfig.json` | `dist/` is gitignored (`typescript/cli/.gitignore`) but published | `typescript/cli/package.json:L27-L31` |

Supported platforms (closed set): `darwin_x86_64`, `darwin_arm64`, `linux_x86_64`, `linux_arm64`, `windows_x86_64`; Windows binary gets `.exe` suffix; `ia32` maps to `i386` and is rejected by the supported-platform check (`typescript/cli/src/platform.ts:L15-L75`, `typescript/cli/src/types.ts:L2-L7`).

#### Root workspace files

| field | value | citation |
|---|---|---|
| root `package.json.version` | `0.0.64` (kept in sync) | `package.json:L18` |
| root `dependencies` | `@erpc-cloud/config: workspace:*`, `esbuild ^0.27.3` (JS-side; the binary embeds Go esbuild v0.27.3 per `go.mod:L18`) | `package.json:L10-L13` |
| root `scripts.build` | `pnpm -r build` (all workspace packages) | `package.json:L3-L5` |
| `packageManager` | `pnpm@10.28.2`; `engines.node >= 18.14` | `package.json:L14-L17` |
| `pnpm-workspace.yaml` | `packages: ['typescript/*']` | `pnpm-workspace.yaml:L1-L2` |
| Go JS engine | `github.com/grafana/sobek v0.0.0-20241024150027` | `go.mod:L22` |

#### `tygo.yaml` (generation config)

| field | value | behavior | citation |
|---|---|---|---|
| `packages[0].path` | `github.com/erpc/erpc/common` | only Go package converted | `tygo.yaml:L2` |
| `output_path` | `typescript/config/src/generated.ts` | committed artifact | `tygo.yaml:L3` |
| `flavor` | `yaml` | property names from `yaml:` tags; untagged exported fields are lowercased wholesale (e.g. `ExecState.UpstreamAttempts` → `upstreamattempts`, `typescript/config/src/generated.ts:L1610`) | `tygo.yaml:L4` |
| `type_mappings` | `time.Duration` → `number /* time in nanoseconds (time.Duration) */` | only selector-expr types mappable; ident mappings blocked on tygo PR #74 (comment in file) | `tygo.yaml:L5-L13` |
| `frontmatter` | imports `LogLevel, Duration, ByteSize, Ts*-aliased ConnectorDriverType/ConnectorConfig/UpstreamType/NetworkArchitecture/AuthType/AuthStrategyConfig/EvmNetworkConfigForDefaults, SelectionPolicyEvalFunction, BoolOrString` from `./types` | `BoolOrString` is currently imported but unused by any generated field | `tygo.yaml:L15-L29`, `typescript/config/src/generated.ts:L2-L15` |
| `exclude_files` | 14 files (see L2) | excluded files contribute zero types | `tygo.yaml:L30-L44` |

#### Hand-written TS types (`typescript/config/src/types/generic.ts`)

| type | exact definition | Go counterpart / runtime behavior | citation |
|---|---|---|---|
| `LogLevel` | `"trace" \| "debug" \| "info" \| "warn" \| "error" \| "disabled" \| undefined` | Go `LogLevel string` via `tstype:"LogLevel"`; zerolog-parsed | `typescript/config/src/types/generic.ts:L17-L24` |
| `Duration` | `` `${number}ms` \| `${number}s` \| `${number}m` \| `${number}h` \| number `` | Go `common.Duration`: string → `time.ParseDuration`; **bare number → milliseconds** (int or float) | `typescript/config/src/types/generic.ts:L30-L35`, `common/duration.go:L10-L32` |
| `ByteSize` | `` `${number}kb` \| `${number}mb` \| `${number}b` \| number `` | used by cache policy `minItemSize`/`maxItemSize` (Go `*string` + `tstype:"ByteSize"`); parsed by `util.ParseByteSize` which supports only B/KB/MB (case-insensitive), **no GB** | `typescript/config/src/types/generic.ts:L40-L44`, `common/config.go:L326-L327`, `util/string.go:L25-L54`, `data/cache_policy.go:L23-L37` |
| `NetworkArchitecture` | `"evm"` | Go const `ArchitectureEvm` | `typescript/config/src/types/generic.ts:L49`, `typescript/config/src/generated.ts:L1666-L1667` |
| `ConnectorDriverType` | `"memory" \| "redis" \| "postgresql" \| "dynamodb"` | **missing `"grpc"`** which Go supports (`DriverGrpc`, `ConnectorConfig.grpc`) | `typescript/config/src/types/generic.ts:L54-L58`, `typescript/config/src/generated.ts:L346-L362` |
| `ConnectorConfig` | discriminated union on `driver` with required matching sub-object (`memory`/`redis`/`dynamodb`/`postgresql`), `id: string` required | replaces tygo's loose `ConnectorConfig` via `tstype:"TsConnectorConfig[]"` on `CacheConfig.Connectors`; no `grpc` arm; note `id` optional in Go (`id?` at `generated.ts:L353`) but required here | `typescript/config/src/types/generic.ts:L63-L83`, `typescript/config/src/generated.ts:L302` |
| `UpstreamType` | `"evm"` + 21 `evm+<vendor>` literals (alchemy, blastapi, conduit, drpc, dwellir, envio, etherspot, infura, pimlico, quicknode, llama, thirdweb, repository, superchain, chainstack, tenderly, onfinality, erpc, blockpi, ankr, routemesh) | Go accepts these as endpoint *schemes* in provider conversion; union is missing `evm+blockdaemon` which Go handles | `typescript/config/src/types/generic.ts:L88-L110`, `common/defaults.go:L1245-L1409` |
| `AuthType` | `"secret" \| "jwt" \| "siwe" \| "network"` | **missing `"database"`** (Go `AuthTypeDatabase` exists) | `typescript/config/src/types/generic.ts:L115`, `typescript/config/src/generated.ts:L1337` |
| `AuthStrategyConfig` | `Omit<Gen, "type"\|"network"\|"secret"\|"jwt"\|"siwe"> & ({type:"secret";secret:SecretStrategyConfig} \| {type:"network";secret:NetworkStrategyConfig} \| {type:"jwt";secret:JwtStrategyConfig} \| {type:"siwe";secret:SiweStrategyConfig})` | **buggy**: non-secret arms keep the key name `secret` instead of `network`/`jwt`/`siwe`; the Go decoder expects `network:`/`jwt:`/`siwe:` keys (`common/config.go` AuthStrategyConfig); also omits `database` arm | `typescript/config/src/types/generic.ts:L120-L141`, `typescript/config/src/generated.ts:L1344-L1354` |
| `EvmNetworkConfigForDefaults` | `Omit<EvmNetworkConfig, "chainId">` | used for `networkDefaults.evm`/`upstreamDefaults.evm` via `tstype:"TsEvmNetworkConfigForDefaults"` (chainId meaningless in defaults) | `typescript/config/src/types/generic.ts:L146`, `typescript/config/src/generated.ts:L498` |
| `BoolOrString` | `boolean \| string` | reserved for connector-ID-pattern fields (e.g. `"redis*"`); currently unreferenced in generated output | `typescript/config/src/types/generic.ts:L148-L152` |

#### Selection-policy typing surface (`typescript/config/src/types/policyEval.ts`)

Documented in depth by the selection-policy KB; the package-level facts: `SelectionPolicyEvalFunction = (upstreams: PolicyEvalUpstreamArray, ctx: PolicyEvalContext) => readonly PolicyEvalUpstream[]` (`typescript/config/src/types/policyEval.ts:L590-L593`); `PolicyEvalUpstreamArray` declares ~60 chainable methods; predicate factories (`errorRateAbove`, `latencyDeviationAbove`, …), presets (`PREFER_FASTEST/FRESHEST/LEAST_ERRORS`), combinators (`all/any/not`), helper fns (`methodMatches`, `durationMs`), and the scope/finality constants are declared as **ambient globals** in a `declare global` block (`typescript/config/src/types/policyEval.ts:L595-L736`) so they type-check inside `evalFunc` bodies without imports — at runtime they're installed on `globalThis` by `internal/policy/stdlib/install.go` (REALTIME=1, UNFINALIZED=2, FINALIZED=4, UNKNOWN=8 at `install.go:L74-L77`; NETWORK…NETWORK_METHOD_FINALITY string values at `install.go:L96-L99`).

#### Exported constants (`typescript/config/src/constants.ts` + re-exports)

| export | value | runtime equivalence | citation |
|---|---|---|---|
| `NETWORK` / `NETWORK_METHOD` / `NETWORK_FINALITY` / `NETWORK_METHOD_FINALITY` | `"network"` / `"network-method"` / `"network-finality"` / `"network-method-finality"` (aliases of generated `EvalScope*` consts) | identical strings the Go `EvalScope` enum validates; same names installed as ambient globals in the policy runtime | `typescript/config/src/constants.ts:L13-L27`, `typescript/config/src/generated.ts:L1278-L1297`, `common/config.go:L2333-L2350` |
| `REALTIME` / `UNFINALIZED` / `FINALIZED` / `UNKNOWN` | `1<<0` / `1<<1` / `1<<2` / `1<<3` | bit-flags for chainable `.when(mask, fn)`; values match `internal/policy/stdlib/install.go:L74-L77` | `typescript/config/src/constants.ts:L40-L47` |
| `DataFinalityStateFinalized/Unfinalized/Realtime/Unknown` | `0/1/2/3` (int enum) | JSON-stringified as numbers from TS; Go `UnmarshalYAML` accepts `"finalized"`/`"0"` etc. (numeric forms included), marshals back as names | `typescript/config/src/generated.ts:L1456-L1487`, `common/data.go:L54-L76` |
| `CacheEmptyBehaviorIgnore/Allow/Only` | `0/1/2` | same string-or-number unmarshal pattern | `typescript/config/src/generated.ts:L1488-L1491`, `common/data.go:L98-L116` |
| `RateLimitPeriodSecond…Year` | `0…6` | Go accepts int enum, names, and duration aliases (`"1s"`, `"24h"`, `"7d"`, …); marshals as names | `typescript/config/src/generated.ts:L1004-L1011`, `common/config.go:L1882-L1947` |
| `ScopeNetwork`/`ScopeUpstream`, `ArchitectureEvm`, `UpstreamTypeEvm`, `AuthTypeSecret/Jwt/Siwe/Network`, `EvmNodeType*`, `EvmSyncingState*`, `ConsensusLowParticipantsBehavior*` (4), `ConsensusDisputeBehavior*` (4) | string/int consts re-exported through the barrel | `AuthTypeDatabase` is **not** re-exported (exists only in generated.ts) | `typescript/config/src/index.ts:L34-L86`, `typescript/config/src/generated.ts:L1335-L1340` |

`index.ts` re-exports ~60 named config interfaces (`Config`, `ProjectConfig`, `UpstreamConfig`, `NetworkConfig`, `FailsafeConfig`, all five policy configs, DB connector configs, auth strategy configs, rate-limit configs, server configs, `ProxyPoolConfig`, …) plus the policy-eval types (`typescript/config/src/index.ts:L1-L160`). Symbols present in `generated.ts` but NOT re-exported (e.g. `TracingConfig`, `SharedStateConfig`, `GrpcConnectorConfig`, `ScoreMultiplierConfig`, `UpstreamRoutingConfig`, `StaticResponseConfig`, `DatabaseStrategyConfig`) are still reachable as nested fields of `Config` — they just can't be imported by name.

#### `createConfig`

```ts
export const createConfig = (cfg: Config): Config => { return cfg; };
```
Pure identity at runtime; exists solely to anchor type inference/checking of the literal (`typescript/config/src/index.ts:L162-L168`). Canonical usage: `export default createConfig({...})` — the default export is what the loader consumes; `createConfig` itself is optional (the Go test passes a bare `export default {...}` object, `common/config_test.go:L152-L181`).

#### Loader environment contract (TS configs)

| facility | behavior | citation |
|---|---|---|
| `process.env.X` | populated from `os.Environ()` at runtime creation; `.env` file loaded first by godotenv in `cmd/erpc` init | `common/runtime.go:L23-L36`, `cmd/erpc/main.go:L41-L46` |
| `env` global | raw `os.Environ()` string array | `common/runtime.go:L19-L21` |
| `console.log/info/warn/debug/trace` | forwarded to zerolog at the matching level (`log`→info); suppressed below global level | `common/console.go:L13-L40` |
| `${VAR}` text expansion | **YAML-only** (`os.ExpandEnv` on raw bytes before YAML decode at `common/config.go:L102`); **never applied to TS/JS files** — the TS loader reads the file bytes verbatim, compiles them, and executes the result in a sobek runtime where `process.env` is pre-populated from `os.Environ()`. TS users must use JS template literals (`` `alchemy://${process.env.ALCHEMY_API_KEY}` ``) or variable lookups (`process.env.MY_URL`). `${VAR}` inside a TS string literal is standard JS template-literal syntax (`${}` interpolation), not shell-style env expansion — they are only equivalent when the author explicitly writes `${process.env.VAR}`. | `common/config.go:L95-L109` (dispatch), `common/config.go:L102` (YAML-only expansion), `common/runtime.go:L19-L36` (TS runtime env setup) |
| Node stdlib (`fs`, `http`, …) | NOT available — sobek is a bare ECMAScript runtime; esbuild will bundle pure-JS npm deps but Node built-ins won't resolve at runtime | `common/runtime.go:L15-L44` (only env/process/console installed) |

#### TypeScript tooling-only env vars (not eRPC server runtime)

These environment variables are consumed exclusively by the TypeScript CLI/release tooling and are **never read by the Go eRPC server process**.

| env var | consumer | required? | behavior | citation |
|---|---|---|---|---|
| `CI` | `@erpc-cloud/cli` postinstall hook | no | When truthy, the postinstall script **skips** the binary download entirely (`if (!process.env.CI) install()`). Allows `npm install` in CI pipelines without fetching a binary (e.g. the release workflow uses `--ignore-scripts` to achieve the same effect). Standard CI platforms (GitHub Actions, CircleCI, etc.) set this automatically. | `typescript/cli/src/install.ts:L111` |
| `VERSION` | release script (`generate-release-files.ts`) | yes (during release) | Semver tag for the release being cut (e.g. `"v0.0.65"`). Written verbatim into `typescript/cli/src/generated/release.ts` as `RELEASE_INFO.version`, which the install script uses to construct the binary download URL (`https://github.com/erpc/erpc/releases/download/<VERSION>/<binaryName>`). Script throws `"VERSION and COMMIT_SHA and CHECKSUMS_FILE environment variables are required"` if absent. | `typescript/cli/src/script/generate-release-files.ts:L65`, `typescript/cli/src/install.ts:L73` |
| `COMMIT_SHA` | release script (`generate-release-files.ts`) | yes (during release) | Full Git commit SHA for the release. Written into `typescript/cli/src/generated/release.ts` as `RELEASE_INFO.commitSha`. Used for traceability of the published npm package back to a specific commit. | `typescript/cli/src/script/generate-release-files.ts:L66`, `typescript/cli/src/generated/release.ts:L5-L6` |
| `CHECKSUMS_FILE` | release script (`generate-release-files.ts`) | yes (during release) | Filesystem path to the `checksums.txt` produced by goreleaser (lines of `<sha256>  <filename>`). The script parses this file, maps each `erpc_<version>_<os>_<arch>` filename to a `SupportedPlatform` key, and writes the resulting map into `typescript/cli/src/generated/checksums.ts` as `CHECKSUMS`. Fails with `"Missing checksums for some platforms"` if fewer than 5 platforms are present. | `typescript/cli/src/script/generate-release-files.ts:L67,L76-L91`, `typescript/cli/src/generated/checksums.ts` |

The release workflow sets all three (`VERSION`, `COMMIT_SHA`, `CHECKSUMS_FILE`) together in the prepare-release job step that invokes `npx ts-node generate-release-files.ts` (`.github/workflows/release.yml:L93-L101`). The resulting generated files are committed as part of the `chore: release <ver>` PR. `CHECKSUMS` data is published in the npm package and is used by `verifyChecksum()` (`typescript/cli/src/checksum.ts:L10-L36`), though that call is currently commented out (`typescript/cli/src/install.ts:L82-L85`).

### Behaviors & algorithms

**Config file discovery order** (`cmd/erpc/main.go:L273-L322`): explicit `--config <file>` flag (forces require-config), else first positional arg (also forces require-config), else probe in order: `./erpc.yaml`, `./erpc.yml`, `./erpc.ts`, `./erpc.js`, then the same four at `/`, then at `/root/` — **YAML wins over TS in the same directory**. If nothing is found and `--require-config` is unset, a default config is synthesized (`cfg.SetDefaults`) with a default `main` project, optionally seeded by repeatable `--endpoint`/`-e` URLs (`cmd/erpc/main.go:L324-L352`, `common/defaults.go:L44-L47,L100-L127`). Subcommands `start` (also the bare default action), `validate --format json|md` (exit 1 on errors; live-probes upstreams), and `dump --format yaml|json` (resolves effective selection policies incl. TS sentinel→source) all share this resolution (`cmd/erpc/main.go:L96-L243`).

**tsLoaderWalker algorithm** (`common/config.go:L2631-L2658`): `walk(node)`: nil → return; Array → recurse into elements; non-object → return; object → for each own property: function value → register `__erpcFns['fn_'+counter++]` + stamp `__erpcFnId`; object value → recurse. Entry point `walk(exports.default)`. Consequences: (a) ids are assigned in deterministic traversal order, keeping load-time sentinels in lockstep with every pool runtime that re-runs the program (`common/config.go:L2627-L2630`); (b) a function appearing **directly as an array element** is *not* registered (functions fail the `typeof node !== 'object'` guard inside `walk`), so the stringify replacer drops it (`undefined` → `null` inside arrays).

**Sentinel format**: `__ts_fn__:<id>` (`tsFunctionSentinelPrefix`, `common/config.go:L2613`); helpers `IsTSFunctionSentinel` / `TSFunctionSentinelID` (`common/config.go:L2771-L2779`). The sentinel lands in `SelectionPolicyConfig.EvalFunc string` (`common/config.go:L2382`) — the only function-typed config leaf today, though the walker/registry mechanism is generic.

**Go→TS mapping rules** (tygo yaml flavor + this repo's conventions), each verified against output:
1. Property name = `yaml` tag name (`HttpPortV4` → `httpPortV4`; `common/config.go:L140` → `typescript/config/src/generated.ts:L150`).
2. `omitempty` and/or pointer → optional `?` (`listenV4?: boolean`, `generated.ts:L145`); non-omitempty stays required even when Go later defaults it (`AliasingConfig.rules`, `generated.ts:L258-L260`; `MemoryConnectorConfig.maxItems/maxTotalSize`, `generated.ts:L369-L373`).
3. `[]*T` → `(T | undefined)[]` (`projects?: (ProjectConfig | undefined)[]`, `generated.ts:L138`).
4. `map[string]V` → `{ [key: string]: V}` (`responseHeaders`, `generated.ts:L170`).
5. `interface{}` → `any` (`DirectiveDefaultsConfig.skipCacheRead?: any`, `generated.ts:L1064`, from `common/config.go:L2108`).
6. Numeric Go types annotated inline: `number /* int64 */`, `number /* float64 */` etc.
7. `time.Duration` (selector expr) → `number /* time in nanoseconds (time.Duration) */` via type_mappings (`MockConnectorConfig.getdelay`, `generated.ts:L376`).
8. `common.Duration` fields require an explicit `tstype:"Duration"` tag to become the friendly template-literal union; untagged ones (e.g. `ProjectConfig.ScoreMetricsWindowSize`, `common/config.go:L533`) still render as `Duration` only because the Go type name coincides with the imported alias — i.e. the local `Duration` import shadows tygo's emitted reference (`generated.ts:L461`).
9. String enums: `type X string` + consts → `export type X = string;` + `export const Xxx: X = "..."` (not a literal union — IDE narrowing comes from extra `tstype` literal unions like `evalScope`, `common/config.go:L2365` → `generated.ts:L1314`).
10. Int enums: consts with numeric values (`DataFinalityState`, `RateLimitPeriod`).
11. `tstype:"-"` removes a field from TS entirely (`evalPerMethod`/`evalPerFinality`, `common/config.go:L2372-L2375` — absent from `generated.ts:L1306-L1324`).
12. `yaml:"-" json:"-"` fields never appear (`Config.UserScript`, `SelectionPolicyConfig.CompiledProgram/DisableTickerForTest/EvalFuncOriginal/LegacySelectionPolicy`).
13. Go doc comments are copied verbatim as JSDoc (e.g. the whole `EvmServedTipConfig` comment block, `generated.ts:L1208-L1241`).
14. Non-config runtime types degrade: interfaces with no yaml tags get lowercased property names (`ExecState`/`UpstreamAttempt`, `generated.ts:L1561-L1632`); pure-runtime types collapse to `any` (`EvmStatePoller`, `CacheDAL`, `HealthTracker`, `Network`, `Upstream` — `generated.ts:L67,L116,L1668,L1704-L1705`).
15. Generated file sections map to `common/` source files: adaptive_duration.go, architecture_evm.go, cache_dal.go, cache_mock.go, config.go, data.go, error_extractor.go, exec_state.go, network.go, timeout_func.go, upstream.go, user.go (`generated.ts` `// source:` markers at L18,L51,L114,L119,L126,L1454,L1501,L1517,L1664,L1673,L1682,L1708).

**Int-enum round-trip**: TS constants serialize as JSON numbers; the YAML decoder feeds them to `UnmarshalYAML` which (for `DataFinalityState`) decodes the scalar into a string and matches `"0"…"3"` (`common/data.go:L54-L76`); `RateLimitPeriod` additionally tries plain int first (`common/config.go:L1882-L1894`). Marshal direction (e.g. `erpc dump`) always emits the human-readable names (`common/data.go:L46-L52`, `common/config.go:L1873-L1879`).

**npm CLI flow**: `getPlatform()` maps `os.type()`/`os.arch()` to the 5-platform set (`typescript/cli/src/platform.ts:L15-L53`); `install()` downloads (following 301/302 redirects) to `dist/bin/erpc_<platform>` and chmods 0755 (`typescript/cli/src/install.ts:L34-L91`); checksum verification implemented (`typescript/cli/src/checksum.ts:L10-L36`, SHA-256 vs `CHECKSUMS[platform]`) but the call is commented out pending a mismatch root-cause (`typescript/cli/src/install.ts:L82-L85`). `generate-release-files.ts` parses goreleaser `checksums.txt` lines, maps filenames matching `_(darwin|linux|windows)_(arm64|x86_64|amd64)` (amd64→x86_64) and fails when fewer than 5 platforms are present (`typescript/cli/src/script/generate-release-files.ts:L8-L91`).

**Release pipeline** (`.github/workflows/release.yml`): `workflow_dispatch(version_tag)` → prepare-release job: tygo generate (L44-47) → goreleaser snapshot for checksums (L52-61) → `pnpm install --ignore-scripts` + `pnpm run build` sanity (L76-82) → `npm version` in `.`, `typescript/cli`, `typescript/config` (L84-91) → generate CLI release files with VERSION/COMMIT_SHA/CHECKSUMS_FILE env (L93-101) → PR `chore: release X` (L107-123). Merge to main with that commit message → release job: force-tag, goreleaser real release with provenance attestation (L168-186), publish `@erpc-cloud/cli` then sed-rename + publish `start-rpc` and `start-erpc`, then publish `@erpc-cloud/config` — each with `|| true` so **publish failures don't fail the workflow** (L206-251). Separate jobs build/push multi-arch Docker images tagged `main`, `<version>`, `latest` (L253-523).

**Canonical example: `erpc.dist.ts`** (`erpc.dist.ts:L1-L97`). Demonstrates: `createConfig` + finality const imports (`DataFinalityStateFinalized/Realtime/Unfinalized`); `logLevel: "trace"`; `server {listenV4, httpHostV4, httpPort: 4000}` (the deprecated `httpPort` key, not `httpPortV4` — still accepted, `common/config.go:L139`); memory cache connector `{maxItems: 100000, maxTotalSize: "1GB"}` + two policies (`finality: DataFinalityStateUnfinalized, ttl: '10s'` and `finality: DataFinalityStateFinalized, ttl: '0s'`); one `main` project, network `evm:1` with an **ordered failsafe list** — `matchFinality: [DataFinalityStateRealtime]` with 3s timeout/2 retries, `matchMethod: "eth_getLogs"` with 5s/5, catch-all `"*"` with 10s/3; three upstreams using provider-scheme endpoints built from env (`` `alchemy://${process.env.ALCHEMY_API_KEY}` ``, `` `chainstack://${...}` ``, `` `${process.env.CUSTOM_RPC_1}` ``). Verified to load + validate clean when those env vars are set (`go run ./cmd/erpc validate --config erpc.dist.ts` → `"errors": []`; provider-scheme endpoints are converted to providers so `upstreamsTotal` counts only the raw-URL upstream).

**Canonical example: `erpc.dist.yaml`** (`erpc.dist.yaml:L1-L255`). Demonstrates: `logLevel: warn`; all four cache connectors (memory, postgresql w/ `connectionUri`, redis w/ `addr`, dynamodb pointed at ScyllaDB Alternator `http://localhost:8067` with `region: DC1`) + three policies routing realtime→memory (2s), unfinalized→redis (10s), finalized→scylladb (ttl 0) all with `empty: allow`; server `httpHostV4/httpPortV4/maxTimeout: 50s` + commented V6 keys; `metrics {enabled, hostV4, port: 4001}`; project-level `scoreMetricsWindowSize: 10s` and `upstreamDefaults.evm.statePollerInterval: 2s` (with long operator-guidance comments); two networks (evm:1, evm:42161) with full failsafe blocks (timeout 30s; retry maxAttempts/delay/backoffMaxDelay/backoffFactor/jitter; hedge delay+maxCount); nine upstreams mixing provider schemes (`alchemy://`, `tenderly://`, `chainstack://`, `onfinality://`, `infura://${YOUR_INFURA_API_KEY}` — YAML env expansion) and raw URLs with explicit `type: evm` + `evm.chainId`; per-upstream `rateLimitBudget` references; six rate-limit budgets with `method: '*'` rules (`maxCount`, `period: 1s` — duration-alias form of `second`). **This file is currently broken as shipped** — see gotcha #2.

### Observability

- **No Prometheus metrics** are owned by this subsystem (nothing in `telemetry/metrics.go` references config loading); the TS loader runs before telemetry exists.
- **Log messages** (zerolog): `"looking for config file: <path>"` and `"resolved configuration file to: <path>"` during discovery (`cmd/erpc/main.go:L316,L342`); `"executing command"` with `action`/`version`/`commit` fields (`cmd/erpc/main.go:L257-L261`); `"failed to load configuration"` on any load error (`cmd/erpc/main.go:L265`); `"failed to load .env file"` for non-ENOENT godotenv errors (`cmd/erpc/main.go:L44`); `"no projects found in config; will add a default 'main' project"` warn (`common/defaults.go:L101`); legacy-migration warnings logged with `source=config-migration` (`cmd/erpc/main.go:L37-L39`).
- **User-script logging**: `console.*` inside a TS config (or evalFunc) emits through the global zerolog logger at the mapped level — usable as a debugging channel for config evaluation (`common/console.go:L24-L40`).
- **Error strings** greppable by operators: `"build failed:"`, `"compile ts config:"`, `"ts config json-stringify:"`, `"config default export serialized to null/undefined"`, `"ts config decode:"` (`common/compiler.go:L32`, `common/config.go:L2700,L2744,L2747,L2758`); `"ts selectionPolicy.evalFunc lookup: …"` family (`internal/policy/eval.go:L606-L621`); `"evaluate user TS config in policy runtime:"` (`internal/policy/runtime_pool.go:L69`).

### Edge cases & gotchas

1. **Default export is mandatory and must be reachable**: missing/undefined/null default export fails with the exact message at `common/config.go:L2715-L2717`. The esbuild IIFE assigns the module to global `exports`; the loader reads `exports.default` (`common/compiler.go:L18`, `common/runtime.go:L54-L56`).
2. **`erpc.dist.yaml` fails validation as shipped**: upstreams reference `rateLimitBudget: global` (`erpc.dist.yaml:L109,L213`) but no `global` budget exists in `rateLimiters.budgets` (`erpc.dist.yaml:L223-L255`). Verified: `go run ./cmd/erpc validate --config erpc.dist.yaml` → `"upstream.*.rateLimitBudget 'global' does not exist in config.rateLimiters"`, exit 1. It additionally contains a duplicate upstream id `infura-multi-chain-example-1` (`erpc.dist.yaml:L155,L211`) which would trip `project.*.upstreams.*.id must be unique` (`common/validation.go:L603`).
3. **TS/JS configs do NOT receive `os.ExpandEnv` treatment — secrets must be injected via `process.env`**: `LoadConfig` dispatches on file suffix at `common/config.go:L95-L109`: for `.ts`/`.js` files it calls `loadConfigFromTypescript` directly, **bypassing the `os.ExpandEnv` call** at `common/config.go:L102` that is applied to YAML/YML byte content before decoding. The raw TS/JS file bytes are passed as-is to esbuild for bundling, then the resulting IIFE is executed in a sobek runtime where `process.env` is pre-populated with `os.Environ()` entries (`common/runtime.go:L23-L36`). Consequence: `${VAR}` written inside a YAML file expands the shell variable before the YAML decoder sees it; `${process.env.VAR}` in a TS template literal expands the JS variable at JS runtime. Users copying a YAML `${MY_SECRET}` pattern into a TS file verbatim get a JS template literal that evaluates `process.env` only if written as `${process.env.MY_SECRET}` — omitting `process.env.` makes it a reference to a JS variable named `MY_SECRET`, which is `undefined` unless declared elsewhere, so the resulting string literal becomes `"undefined"` (e.g. `alchemy://undefined`), producing confusing downstream errors (verified: validate without env vars fails with `"unsupported vendor name in vendor.settings"`). YAML's `os.ExpandEnv` expands unset vars to empty string (Go stdlib contract), while TS gets the JS `undefined`-coercion to `"undefined"`. See also: gotcha #14 (YAML beats TS in discovery — a stale `erpc.yaml` can suppress the TS file entirely, making env-expansion differences a non-issue but silently hiding the TS config).
4. **TS-required fields that Go would default**: tygo marks non-`omitempty` fields required, so e.g. `MemoryConnectorConfig` forces `maxItems` + `maxTotalSize` in TS (`typescript/config/src/generated.ts:L369-L373`) even though Go defaults them to `100000` / `"1GB"` (`common/defaults.go:L935-L944`). YAML users can omit them (the dist YAML does, `erpc.dist.yaml:L5-L6`).
5. **ByteSize unit mismatch**: `maxTotalSize` is typed plain `string` and parsed by `humanize.ParseBytes` (GB/GiB fine — `data/memory.go:L54`), but cache-policy `minItemSize`/`maxItemSize` use the `ByteSize` union (no `gb` form) and Go parses them with `util.ParseByteSize` which only understands B/KB/MB and errors on GB (`util/string.go:L25-L54`).
6. **Hand-written `AuthStrategyConfig` union is wrong for non-secret strategies**: the `network`/`jwt`/`siwe` arms declare their payload under the key `secret` (`typescript/config/src/types/generic.ts:L124-L141`), while the Go struct expects `network:`/`jwt:`/`siwe:` keys (`typescript/config/src/generated.ts:L1344-L1354`). A correctly-shaped `{type:"jwt", jwt:{...}}` literal fails TS excess-property checks; a type-passing `{type:"jwt", secret:{...}}` is wrong at runtime. The `database` auth type is absent from the TS union and from `index.ts` exports entirely (Go: `generated.ts:L1337,L1363-L1384`).
7. **`grpc` cache-connector driver is unrepresentable in TS**: Go supports `DriverGrpc` + `ConnectorConfig.grpc` (`typescript/config/src/generated.ts:L351,L359`), but the hand-written `TsConnectorConfig` union used for `cache.connectors` has no grpc arm (`typescript/config/src/types/generic.ts:L63-L83`). Same for `UpstreamType` missing `evm+blockdaemon` (`common/defaults.go:L1385`).
8. **Functions outside walkable positions are silently dropped**: the JSON replacer returns `undefined` for any function without `__erpcFnId` (`common/config.go:L2734-L2738`), and the walker registers only function-valued *object properties* — a function that is a direct array element is skipped by the `typeof node !== 'object'` guard (`common/config.go:L2635-L2652`). Only `selectionPolicy.evalFunc` is a legitimate function leaf today.
9. **Stale doc comments propagate from Go into the published `.d.ts`**: `ProjectConfig.ScoreMetricsWindowSize`'s comment claims "Defaults to 10m when zero" (`common/config.go:L530`, copied to `typescript/config/src/generated.ts:L457`) but the actual fallback is `1 * time.Minute` (`erpc/projects_registry.go:L50,L133-L134`); `erpc.dist.yaml:L61` claims "Defaults to 5s". Three different claims; the code says 1m.
10. **`main: "index.js"` in `@erpc-cloud/config` is a dangling path** (no root `index.js`; the bundle is `lib/index.js`). Modern Node/bundlers use the `exports` map and work; legacy `main`-only resolvers break (`typescript/config/package.json:L9,L23-L30`).
11. **tygo cannot remap ident types** — the commented-out `type_mappings` entries and the `Ts*` frontmatter-alias workaround exist because of pending tygo PR #74 (`tygo.yaml:L6-L13`). Adding a new weakly-typed Go field means adding a `tstype:` tag + (possibly) a frontmatter import, or the TS side gets `string`/`any`.
12. **CLI checksum verification is disabled**: download integrity is currently *not* checked at install (commented call + TODO at `typescript/cli/src/install.ts:L82-L85`); the full SHA-256 machinery exists (`typescript/cli/src/checksum.ts`) and CI-generated `CHECKSUMS` are published, so re-enabling is one uncomment away. Release artifacts are separately attested via GitHub build provenance (`.github/workflows/release.yml:L183-L186`).
13. **`postinstall` skips in CI** (`if (!process.env.CI) install()`, `typescript/cli/src/install.ts:L111`) and the release workflow installs with `--ignore-scripts` (`release.yml:L76-L79`) — local installs in CI environments will have no binary until `node dist/install.js` is run manually. `VERSION`, `COMMIT_SHA`, and `CHECKSUMS_FILE` are **release-pipeline-only** env vars read by `typescript/cli/src/script/generate-release-files.ts:L65-L67`; they are not eRPC server runtime vars and are never read by Go code. All four (`CI`, `VERSION`, `COMMIT_SHA`, `CHECKSUMS_FILE`) are fully documented in the "TypeScript tooling-only env vars" table above.
14. **YAML beats TS in auto-discovery**: `./erpc.yaml`/`./erpc.yml` are probed before `./erpc.ts`/`./erpc.js` (`cmd/erpc/main.go:L279-L294`). A leftover erpc.yaml silently shadows a newly added erpc.ts.
15. **The whole TS module executes multiple times** — once at load (`common/config.go:L2710`) and once per policy-pool runtime acquire (`internal/policy/runtime_pool.go:L67-L71`), plus once more for `erpc dump` sentinel resolution (`internal/policy/dump.go:L65-L70`). Top-level side effects (logging, expensive computation) are repeated; the config should stay pure.
16. **`evalPerMethod`/`evalPerFinality` are deliberately invisible to TS** (`tstype:"-"`, `common/config.go:L2372-L2375`); the canonical knob is `evalScope`, whose tstype union re-adds the literal strings for IDE narrowing (`common/config.go:L2365`). TS string-form `evalFunc` is still legal (`evalFunc?: SelectionPolicyEvalFunction | string`, `generated.ts:L1323`; type-test `typescript/config/src/__tests__/erpc.test-d.ts:L63-L82`).
17. **Strict decode applies to TS configs too**: `decoder.KnownFields(true)` on the JSON intermediary (`common/config.go:L2756`) means a property that type-checks via index-signature trickery (or `as any`) still fails at load with the same unknown-field error YAML gives (test for YAML-side messages: `common/config_test.go:L947-L1136`).
18. **Type-level regression suite encodes the public contract**: `typescript/config/src/__tests__/erpc.test-d.ts` (run by `pnpm --filter @erpc-cloud/config test`) asserts: realistic chained evalFunc compiles (L24-61); string evalFunc accepted (L63-82); custom predicates + combinators compose (L86-109); per-upstream weight functions (L113-118); `ctx` field types incl. the note that `previousExcluded`/`excludedSince` were removed with `probeExcluded` (L122-140); `sortByScore` multiplier modes incl. `@ts-expect-error` negatives (L142-165); `routing.scoreMultipliers` config form (L169-194); and six negative cases that must fail compilation (L209-340).
19. **Legacy YAML keys work in TS objects too** because the unified pipeline routes TS through the same UnmarshalYAML shadows — e.g. `group: 'main'` on an upstream becomes a `tier:main` tag (`common/config_test.go:L147-L212`).
20. **Provider-scheme endpoints in examples change resource counts**: `alchemy://…`-style upstream entries are converted to *providers* at SetDefaults (`common/defaults.go:L1206+`), so `validate` reports them under providers, not `upstreamsTotal` (observed in the erpc.dist.ts validation run; relevant when users wonder why upstream counts look low).
21. **`erpc dump` may print `__ts_fn__:fn_N`** for a TS evalFunc when source resolution fails (best-effort `.toString()`; any error leaves the sentinel — `internal/policy/dump.go:L22-L31,L47-L107`).
22. **pnpm publish failures are swallowed** (`|| true` on all four publish steps, `.github/workflows/release.yml:L211,L224,L239,L249`): a release can succeed on GitHub/Docker while npm packages silently lag a version.

### Source map

- `typescript/config/package.json` — npm manifest of `@erpc-cloud/config`; build scripts (esbuild bundle + tsc declarations), exports map, shipped files.
- `typescript/config/src/index.ts` — public barrel: re-exports + the `createConfig` identity helper.
- `typescript/config/src/generated.ts` — tygo output from `common/` (DO NOT EDIT); all config interfaces/enums/consts.
- `typescript/config/src/types/generic.ts` — hand-written refinements: Duration/ByteSize/LogLevel template unions, connector/upstream/auth discriminated unions, `EvmNetworkConfigForDefaults`, `BoolOrString`.
- `typescript/config/src/types/policyEval.ts` — selection-policy eval typing: upstream/ctx shapes, chainable array interface, ambient-global declarations for predicates/presets/constants.
- `typescript/config/src/types/index.ts` — types barrel feeding both `index.ts` and the tygo frontmatter import.
- `typescript/config/src/constants.ts` — CAPITAL_SNAKE evalScope aliases + finality bit-flags (mirrors policy stdlib globals).
- `typescript/config/src/__tests__/erpc.test-d.ts` — compile-time contract tests (`tsc --noEmit`).
- `typescript/config/lib/*` — committed build artifacts (CJS bundle + d.ts + maps).
- `typescript/config/tsconfig.json`, `tsconfig.test.json` — build and type-test compiler configs.
- `typescript/config/README.md` — usage + Docker node_modules mounting/custom-image recipes.
- `typescript/cli/package.json` — npm manifest of `@erpc-cloud/cli` (bin/postinstall/preuninstall hooks).
- `typescript/cli/src/bin.ts` — spawns the downloaded binary, forwards argv/exit code.
- `typescript/cli/src/install.ts` — postinstall binary download (redirect-following, chmod, CI guard, checksum call disabled).
- `typescript/cli/src/platform.ts`, `src/types.ts` — platform detection + supported-platform/checksum types.
- `typescript/cli/src/checksum.ts` — SHA-256 verification helper (currently unused).
- `typescript/cli/src/generated/{release,checksums}.ts` — CI-generated version/commit pin + per-platform checksums.
- `typescript/cli/src/script/generate-release-files.ts` — CI release script: reads `VERSION`, `COMMIT_SHA`, `CHECKSUMS_FILE` env vars; parses goreleaser checksums.txt; writes `src/generated/{release,checksums}.ts`.
- `tygo.yaml` — generation config (package, flavor, mappings, frontmatter, exclusions).
- `package.json`, `pnpm-workspace.yaml` — workspace root; version anchor; `@erpc-cloud/config` symlink dependency.
- `erpc.dist.ts` / `erpc.dist.yaml` — canonical full examples (TS: cache+policies+failsafe-list+provider env endpoints; YAML: 4 connectors, finality-tiered cache policies, 2 networks, 9 upstreams, 6 rate-limit budgets).
- `common/config.go` — `LoadConfig` dispatch (L87-L132), `UserScript` field (L51-L67), sentinel machinery + `loadConfigFromTypescript` (L2605-L2779).
- `common/compiler.go` — embedded esbuild build options; `CompileFunction`/`CompileProgram` for YAML-string evalFuncs.
- `common/runtime.go` — sobek runtime factory (env, process.env, console, exports accessor).
- `common/console.go` — console→zerolog bridge.
- `common/duration.go`, `common/data.go` — scalar coercion rules that make TS numeric/enum values round-trip.
- `cmd/erpc/main.go` — CLI commands, config discovery order, `.env` loading, legacy-translator wiring.
- `internal/policy/runtime_pool.go` — re-runs UserScript per pooled runtime (closure preservation).
- `internal/policy/eval.go` (L587-L621) — sentinel → `__erpcFns` lookup at tick time.
- `internal/policy/dump.go` — sentinel → source resolution for `erpc dump`.
- `common/config_test.go` (L147-L348) — TS unified-pipeline + closure-preservation tests.
- `.github/workflows/release.yml` — tygo regen, version bumps, npm publishes (3 CLI names + config), binary/docker releases.
- `Dockerfile` — ts-dev/ts-prod stages shipping `/typescript` + `/node_modules` into the final image.
