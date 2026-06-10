# KB: Providers & vendors (all third-party integrations)

> status: complete
> source-dirs: thirdparty/ (all files), common/vendors.go, common/config.go (ProviderConfig/VendorSettings), common/defaults.go (shorthand conversion + provider defaults), common/validation.go (ProviderConfig.Validate), upstream/registry.go (lazy provider bootstrap), upstream/upstream.go (vendor attach), architecture/evm/error_normalizer.go (vendor error hook), util/initializer.go, util/ids.go, util/redact.go, util/schemes.go

## L1 — Capability (CTO view)

Providers are eRPC's "bring an API key, get every chain" layer: instead of configuring one upstream per network, an operator declares a single provider (e.g. `vendor: alchemy` + `apiKey`) and eRPC lazily generates concrete upstream configs per network on first request, using each vendor's chain catalog (static map, remote discovery API, or live `eth_chainId` probing). 22 vendor integrations ship built in — Alchemy, dRPC, QuickNode, Chainstack, Infura, Envio HyperRPC, Pimlico/Etherspot (ERC-4337), the OP Superchain registry, a public-endpoints repository, another eRPC instance, and more — each also contributing vendor-specific JSON-RPC error normalization for upstreams it "owns". A one-line URL shorthand (`alchemy://KEY` as an upstream endpoint) expands into a full provider, and a config with zero upstreams/providers automatically falls back to the public `repository` + `envio` providers.

## L2 — Mechanics (staff-engineer view)

**Two distinct concepts.** A *vendor* (`common.Vendor` interface, common/vendors.go:10-16) is stateless integration logic: `Name()`, `OwnsUpstream(cfg)` (endpoint-pattern recognition), `SupportsNetwork(ctx,logger,settings,networkId)`, `GenerateConfigs(ctx,logger,baseUpstreamCfg,settings)` and `GetVendorSpecificErrorIfAny(req,resp,body,details)`. A *provider* (`thirdparty.Provider`, thirdparty/provider.go:13-18) is a configured instance of a vendor: `ProviderConfig{Id, Vendor, Settings, OnlyNetworks, IgnoreNetworks, UpstreamIdTemplate, Overrides}` (common/config.go:669-677). All 22 vendors are registered in a global `VendorsRegistry` built once at startup (thirdparty/vendors_registry.go:9-35; erpc/erpc.go:67); a per-project `ProvidersRegistry` resolves each `providers[].vendor` string via `LookupByName` and fails startup with the supported-vendors list if unknown (thirdparty/providers_registry.go:15-34).

**Lazy upstream generation flow.** When a request arrives for a network with no prepared upstreams, `UpstreamsRegistry.PrepareUpstreamsForNetwork` schedules — once per network via `sync.Once` — one bootstrap task per provider named `network/<networkId>/provider/<providerId>` (upstream/registry.go:164-186, 477-510). Each task: (1) calls `Provider.SupportsNetwork`, which applies `ignoreNetworks` (deny-list) first, then `onlyNetworks` (exhaustive allow-list), then delegates to the vendor (thirdparty/provider.go:33-52); (2) calls `Provider.GenerateUpstreamConfigs`, which builds a base `UpstreamConfig` — copied from the first wildcard-matching `overrides` entry, else fresh from `upstreamDefaults` — stamps `VendorName`, renders the upstream id from `upstreamIdTemplate` (`<VENDOR>`, `<PROVIDER>`, `<NETWORK>`, `<EVM_CHAIN_ID>` placeholders), parses the chain id for `evm:*` networks, hands it to the vendor's `GenerateConfigs`, then runs `os.ExpandEnv` over every generated endpoint (thirdparty/provider.go:54-138); (3) registers the resulting configs as ordinary upstream bootstrap tasks (upstream/registry.go:498-507). The caller waits up to 30s (capped by the request deadline) polling every 200ms until ≥1 upstream is ready; if provider tasks are still running it returns retryable `ErrNetworkInitializing`, and if every provider task finished without producing upstreams it returns `ErrNetworkNotSupported` (upstream/registry.go:188-293). Failed tasks are auto-retried by the initializer with backoff (factor 1.5, 3s→130s; util/initializer.go:179-182).

**Vendor chain-support strategies** (how `SupportsNetwork` answers): (a) static in-code chain-id maps (blastapi, dwellir, infura, llama, onfinality, blockpi, ankr, blockdaemon, etherspot); (b) remote catalog with built-in static fallback on cold start (alchemy, drpc); (c) remote catalog with **no** fallback — cold start returns the retryable sentinel `ErrRemoteCacheCold` so the initializer retries after the async fetch lands (conduit, superchain, tenderly, quicknode, chainstack, repository); (d) live `eth_chainId` probe against the candidate endpoint using a cached headless client built around a `phonyUpstream` stub (envio for unknown chains, pimlico for unknown chains, erpc, routemesh, thirdweb).

**The request-path safety rule.** Any vendor consulting remote data on the hot path must use the shared `RemoteDataCache[T]` (thirdparty/remote_cache.go): reads are a single `atomic.Pointer` load (never a mutex, never I/O); staleness triggers a single-flight async refresh in a goroutine with a self-contained 90s timeout (caller's context deliberately not used); failures keep the previous snapshot and log a warning; publishes are copy-on-write; panics in fetchers are recovered. The file's header comment (remote_cache.go:13-78) documents why: a mutex-around-HTTP in `SupportsNetwork` once head-of-line-blocked the entire proxy when a vendor discovery API hung. Code review must reject mutex+HTTP on sight.

**URL shorthand scheme.** At config-defaults time, any upstream whose endpoint scheme is not `http/https/grpc/grpc+bds` is converted into a ProviderConfig: scheme (minus optional `evm+` prefix) becomes the vendor name, `buildProviderSettings` maps the URL parts into vendor settings (host = apiKey for most vendors; envio→rootDomain, thirdweb→clientId, superchain→registryUrl, repository→repositoryUrl, erpc→endpoint(+secret), routemesh→baseURL+apiKey from path; quicknode/chainstack also parse query params), the original upstream config (sans id/endpoint) becomes a `"*"` override so its failsafe/rate-limit/etc. apply to every generated upstream, and the provider id defaults to `<vendor>-<n>` via a process-wide counter (common/defaults.go:1193-1241, 1243-1425; util/ids.go:32-41). Unknown shorthand scheme = config load error (defaults.go:1424).

**Default providers.** A project with zero upstreams and zero providers (and no CLI endpoints) automatically gets `repository` (public endpoints, id `public`) + `envio` providers, with a warning log (common/defaults.go:1113-1142; CLI endpoints path cmd/erpc/main.go:325-336).

**Vendors also serve statically-configured upstreams.** At `NewUpstream`, `LookupByUpstream` finds a vendor by explicit `upstream.vendorName` (exact match, wins) or the first vendor whose `OwnsUpstream` matches the endpoint (upstream/upstream.go:171; thirdparty/vendors_registry.go:54-71). The vendor's `GenerateConfigs` is then invoked with `settings == nil` to apply vendor defaults (e.g. dRPC forcing `autoIgnoreUnsupportedMethods: false`, envio's method allow-list) and the first returned config replaces the upstream's config (upstream/upstream.go:191-200). If no vendor matches, `vendorName` is guessed as `unknown-<rootDomain>` from the endpoint host (upstream/upstream.go:202-208, 1471-1503). The vendor attached this way powers per-vendor JSON-RPC error mapping: `architecture/evm/error_normalizer.go:29,878-900` consults `upstream.Vendor().GetVendorSpecificErrorIfAny` before generic error normalization, letting e.g. Alchemy's `-32600 …must be authenticated` become `ErrEndpointUnauthorized` and QuickNode's `-32614` become a block-range-too-large error.

**Failure modes.** Provider task errors (cold cache, vendor API down, bad apiKey) are retried forever in the background by the initializer; requests for that network see `ErrNetworkInitializing` until ≥1 upstream registers. Vendors with static fallbacks degrade gracefully (alchemy/drpc serve the built-in map). A provider whose vendor returns `(false, nil)` from `SupportsNetwork` is silently skipped for that network (upstream/registry.go:491-493).

## L3 — Exhaustive reference (agent view)

### Config fields

#### `projects[].providers[]` (ProviderConfig, common/config.go:669-677)

| dotted path | type | default | behavior / interactions | source |
|---|---|---|---|---|
| `providers[].id` | string | the vendor name (`p.Id = p.Vendor`) | Unique handle; appears in the `<PROVIDER>` template placeholder and bootstrap task names `network/<networkId>/provider/<id>`. Validation requires non-empty after defaults. Shorthand-converted providers get `<vendor>-<n>` (n = process-wide counter per vendor name) when the source upstream had no id, else the upstream's id. | common/defaults.go:1474-1476; common/validation.go:781-783; common/defaults.go:1219-1225; util/ids.go:32-41 |
| `providers[].vendor` | string | — (required) | Must exactly equal one of the 22 registered `Name()` strings: `alchemy, blastapi, conduit, drpc, dwellir, envio, etherspot, infura, pimlico, quicknode, llama, thirdweb, repository, superchain, tenderly, chainstack, onfinality, erpc, blockpi, ankr, routemesh, blockdaemon`. Unknown vendor → startup error listing supported vendors. | thirdparty/vendors_registry.go:12-33,45-52; thirdparty/providers_registry.go:23-27; common/validation.go:784-786 |
| `providers[].settings` | map[string]interface{} (`VendorSettings`) | `nil` | Free-form, vendor-interpreted (per-vendor tables below). REDACTED in JSON/YAML marshaling (so secrets never echo via admin/config dump). | common/config.go:667,672,679-700 |
| `providers[].onlyNetworks` | []string | `nil` | Exhaustive allow-list of network ids (`evm:<chainId>` format, each validated by `IsValidNetwork` — positive integer required). If set, provider supports exactly these networks; vendor's own `SupportsNetwork` is *not* consulted. Mutually exclusive with `ignoreNetworks` (validation error if both set). | thirdparty/provider.go:42-49; common/validation.go:790-792,800-806; common/network.go:39-49 |
| `providers[].ignoreNetworks` | []string | `nil` | Deny-list checked *first*: exact string match against the requested networkId returns unsupported. Same `evm:<chainId>` format validation. Omitted from `MarshalJSON` (but present in YAML marshal). | thirdparty/provider.go:34-40; common/validation.go:790-792,807-813; common/config.go:679-688 |
| `providers[].upstreamIdTemplate` | string | `"<PROVIDER>-<NETWORK>"` | Template for generated upstream ids. Placeholders: `<VENDOR>` → `vendor`, `<PROVIDER>` → `id`, `<NETWORK>` → full networkId (e.g. `evm:1`), `<EVM_CHAIN_ID>` → numeric chain id for `evm:` networks, literal `N/A` otherwise. Validation requires non-empty after defaults. | common/defaults.go:1477-1479; thirdparty/provider.go:104-114,141-162; common/validation.go:787-789 |
| `providers[].overrides` | map[string]*UpstreamConfig | `nil` | Keys are wildcard patterns matched against the networkId (full `WildcardMatch` grammar: globs `*`/`?`, `|` OR, `&` AND, `!` NOT, parens). First matching entry (map iteration order — nondeterministic if multiple patterns match!) is **copied** as the base upstream config before the vendor fills the endpoint; pattern errors are skipped silently. If none match, a fresh `UpstreamConfig` gets `SetDefaults(upstreamDefaults)` with `Id` reset to "". Each override itself receives `SetDefaults(upstreamDefaults)` at config load and is validated with the endpoint check skipped (endpoint may legitimately be empty). **Safe-use rule**: because Go map iteration is random, if you have multiple patterns that could both match the same network (e.g. `"evm:1*"` and `"*"`), which override wins is nondeterministic across restarts. The only safe patterns are: (a) a single catch-all `"*"` key that covers every network (always applied when no other matches — but since there is no ordering, if there are multiple keys and any other key can also match, you still hit nondeterminism); (b) disjoint patterns that cannot simultaneously match the same network id (e.g. `"evm:1"` and `"evm:2"` — mutually exclusive). In practice: if you need one config for all networks, use only `"*"`. If you need per-network overrides, use exact `evm:<chainId>` keys (they cannot overlap). Never combine a catch-all `"*"` with a pattern that is also a superset of specific chains without accepting that the result may vary. | thirdparty/provider.go:77-101; common/matcher.go:34-47; common/defaults.go:1480-1486; common/validation.go:793-799 |

Notes that apply to all provider-generated upstreams:
- `VendorName` is force-set to the provider's vendor before the vendor runs (thirdparty/provider.go:102).
- For `evm:<id>` networks the base config gets `Type=evm`, `Evm.ChainId=<id>`, and `Evm.SetDefaults(upstreamDefaults.Evm)` (thirdparty/provider.go:116-135); a non-numeric chain id is a generation error (provider.go:118-121).
- After the vendor returns, every endpoint passes through `os.ExpandEnv` — `$VAR`/`${VAR}` in endpoints (including ones the vendor built from your apiKey) are substituted from the environment (thirdparty/provider.go:63,67-71).

#### Per-vendor `settings` keys (read via type assertion from `VendorSettings`)

⚠ Universal footgun: every `recheckInterval` is read as `settings["recheckInterval"].(time.Duration)`. `VendorSettings` is a plain `map[string]interface{}` with no custom unmarshal (common/config.go:667), so a YAML scalar (e.g. `recheckInterval: "24h"` or `recheckInterval: 86400000`) arrives as a `string` or `int64` after YAML decode — the `.(time.Duration)` type assertion fails **silently** and the vendor default is used. This cannot be worked around in YAML config.

**Workaround A — TypeScript config (`erpc.ts`)**: `VendorSettings` in the TypeScript SDK is typed as `{ [key: string]: any }` (typescript/config/src/generated.ts:509). The TS `Duration` type (`number | "${n}ms" | "${n}s" | "${n}m" | "${n}h"`) maps to whatever value you pass. Because the TS config pipeline serializes the config to JSON and the Go side `json.Unmarshal`s into `map[string]interface{}`, a JSON number decodes as `float64`, not `time.Duration`, so the same assertion still fails. There is **no automatic Duration→time.Duration coercion** in the `VendorSettings` path. The only working approach from TS config is to pass a `number` and accept that it will be silently ignored — unless you patch `VendorSettings` unmarshaling in the future.

**Workaround B — Go programmatic config**: Construct `common.VendorSettings{"recheckInterval": 24 * time.Hour}` directly in Go. Because `time.Duration` is just `int64`, the type assertion `.(time.Duration)` succeeds. This is the only fully functional path for custom recheck intervals. Example: `Settings: common.VendorSettings{"apiKey": "...", "recheckInterval": 12 * time.Hour}` in a `ProviderConfig` constructed in Go before passing to eRPC's `NewErpc`. (thirdparty/alchemy.go:205-208, quicknode.go:114-117, repository.go:56-59, drpc.go:226-229, conduit.go:63-66, superchain.go:132-135, tenderly.go:56-59, chainstack.go:88-91)

| vendor | key | required? | default | meaning | source |
|---|---|---|---|---|---|
| alchemy | `apiKey` | yes (when endpoint not preset) | — | inserted into `https://{subdomain}.g.alchemy.com/v2/{apiKey}` | thirdparty/alchemy.go:221-224,256 |
| alchemy | `chainsUrl` | no | `https://app-api.alchemy.com/trpc/config.getNetworkConfig` | network-discovery tRPC URL; structurally validated (http/https + host) else error | thirdparty/alchemy.go:158,196-203,235-242; thirdparty/vendor_utils.go:11-23 |
| alchemy | `recheckInterval` | no | `24h` (`DefaultAlchemyRecheckInterval`) | snapshot freshness window | thirdparty/alchemy.go:154,205-208,244-247 |
| blastapi | `apiKey` | yes | — | `https://{netName}.blastapi.io/{apiKey}` | thirdparty/blastapi.go:128-153 |
| conduit | `apiKey` | yes | — | appended to discovered `httpEndpoint`: `{httpEndpoint}/{apiKey}` | thirdparty/conduit.go:121-124,146 |
| conduit | `networksUrl` | no | `https://api.conduit.xyz/public/network/all` | discovery URL | thirdparty/conduit.go:16,58-61,126-129 |
| conduit | `recheckInterval` | no | `24h` | freshness window | thirdparty/conduit.go:17,63-66,131-134 |
| drpc | `apiKey` | yes | — | `https://lb.drpc.org/ogrpc?network={name}&dkey={apiKey}` | thirdparty/drpc.go:258-261,292 |
| drpc | `chainsUrl` | no | `https://lb.drpc.org/networks` | discovery URL; validated like alchemy's | thirdparty/drpc.go:173,217-224,271-278 |
| drpc | `recheckInterval` | no | `24h` (`DefaultDrpcRecheckInterval`) | freshness window | thirdparty/drpc.go:175,226-229,280-283 |
| dwellir | `apiKey` | yes | — | `https://{subdomain}.dwellir.com/{apiKey}` | thirdparty/dwellir.go:130-133,148 |
| envio | `rootDomain` | no | `rpc.hypersync.xyz` (`DefaultEnvioRootDomain`) | `https://{chainId}.{rootDomain}[/{apiKey}]` | thirdparty/envio.go:20,115-118,197-200,228-240 |
| envio | `apiKey` | no | `""` (omitted from URL) | appended as path segment when non-empty | thirdparty/envio.go:120,201,230-234 |
| etherspot | `apiKey` | yes | — | appended as `?apikey={apiKey}` unless literal `"public"` (or empty) which omits the param | thirdparty/etherspot.go:96-110,136-138 |
| infura | `apiKey` | yes | — | `https://{netName}.infura.io/v3/{apiKey}` | thirdparty/infura.go:80-103 |
| pimlico | `apiKey` | yes (even for SupportsNetwork; error if missing) | — | `"public"` → `https://public.pimlico.io/v2/{chainId}/rpc`; else `https://api.pimlico.io/v2/{chainId}/rpc?apikey={apiKey}` | thirdparty/pimlico.go:120-123,175-187,223-235 |
| quicknode | `apiKey` | yes | — | QuickNode account API key for `https://api.quicknode.com/v0/endpoints` (header `x-api-key`); SupportsNetwork returns `(false,nil)` without it | thirdparty/quicknode.go:109-112,140-143,246 |
| quicknode | `recheckInterval` | no | `1h` (`DefaultQuicknodeRecheckInterval`) | freshness window | thirdparty/quicknode.go:44,114-117,152-155 |
| quicknode | `tagIds` | no | `nil` | **QuickNode tag filter by numeric ID**. QuickNode allows operators to attach user-defined "tags" (metadata labels) to their provisioned endpoints in the QuickNode dashboard. Tags are useful to group or mark endpoints (e.g. "production", "archive", "team-a"), and QuickNode's API supports filtering the endpoint list by those tags. `tagIds` accepts one or more integer tag IDs (accepts `int`, `[]int`, or `[]interface{}` of ints); sent as `?tag_ids=1,2,3` query param to the QuickNode discovery API (`https://api.quicknode.com/v0/endpoints`). Using `tagIds`/`tagLabels` lets you provision multiple providers with the same `apiKey` but restrict each to a different subset of your QuickNode fleet. **Critical footgun**: the RemoteDataCache key is `apiKey` only (quicknode.go:185), not `apiKey+tags`. Two providers with the same `apiKey` but different `tagIds`/`tagLabels` share one endpoint snapshot — whichever provider triggers the first async refresh determines what both see (quicknode.go:184-205). Use different apiKeys per logical group to avoid this. | thirdparty/quicknode.go:56-92,207-237; quicknode_test.go:10-41 |
| quicknode | `tagLabels` | no | `nil` | **QuickNode tag filter by label string**. Same concept as `tagIds` but filters by tag label strings (the human-readable names assigned in the QuickNode dashboard). Accepts `string`, `[]string`, or `[]interface{}` of strings; sent as `?tag_labels=prod,archive` query param (comma-joined). Same cache-key sharing footgun as `tagIds` — both filters are extracted at refresh time only and the cache is keyed by `apiKey` alone. | thirdparty/quicknode.go:76-89,234-237 |
| llama | `apiKey` | yes | — | `https://{netName}.llamarpc.com/{apiKey}` | thirdparty/llama.go:53-66 |
| thirdweb | `clientId` | yes (also for SupportsNetwork; error if missing) | — | `https://{chainId}.rpc.thirdweb.com/{clientId}` | thirdparty/thirdweb.go:45-48,99-107,125-132 |
| repository | `repositoryUrl` | no | `https://evm-public-endpoints.erpc.cloud` (`DefaultRepositoryURL`) | JSON map `{ "<chainId>": { "endpoints": [...] } }` | thirdparty/repository.go:17,51-54,103-106,216-229 |
| repository | `recheckInterval` | no | `1h` (`DefaultRecheckInterval`) | freshness window | thirdparty/repository.go:18,56-59,108-111 |
| superchain | `registryUrl` | no | `https://raw.githubusercontent.com/ethereum-optimism/superchain-registry/main/chainList.json` | flexible spec, see superchain subsection | thirdparty/superchain.go:16,122-125,195-198 |
| superchain | `recheckInterval` | no | `24h` (`DefaultSuperchainRecheckInterval`) | freshness window | thirdparty/superchain.go:17,132-135,205-208 |
| tenderly | `apiKey` | yes | — | `https://{slug}.gateway.tenderly.co/{apiKey}` | thirdparty/tenderly.go:92-95,121 |
| tenderly | `recheckInterval` | no | `24h` (`DefaultTenderlyRecheckInterval`) | freshness window; the chains URL itself is a hardcoded const (`https://api.tenderly.co/api/v1/supported-networks`) — NOT overridable | thirdparty/tenderly.go:34-35,56-59,106-109 |
| chainstack | `apiKey` | yes (SupportsNetwork returns `(false,nil)` without it) | — | platform API key, `Authorization: Bearer` to `https://api.chainstack.com/v1/nodes/` | thirdparty/chainstack.go:83-86,135-138,271 |
| chainstack | `recheckInterval` | no | `1h` (`DefaultChainstackRecheckInterval`) | freshness window | thirdparty/chainstack.go:59,88-91,149-152 |
| chainstack | `project` / `organization` / `region` / `provider` / `type` | no | `""` each | **Chainstack API-side node filters**. These values are forwarded verbatim as query parameters to `GET https://api.chainstack.com/v1/nodes/` (`project=`, `organization=`, `region=`, `provider=`, `type=`), which causes the Chainstack platform to return only nodes matching those criteria. They are NOT eRPC-internal filters — eRPC never re-checks them locally. All non-empty values participate in the RemoteDataCache key `{apiKey}[_p:project][_o:org][_r:region][_pr:provider][_t:type]` (chainstack.go:211-230) so two providers sharing an apiKey but different filter combinations get separate endpoint snapshots. **Accepted values**: Chainstack platform IDs/names (e.g. region slug like `us-east-1`, provider name like `aws`, node type like `dedicated`). Consult Chainstack's platform API docs for the enumeration; eRPC passes them through without validation. **`type` field name collision warning**: the settings key `"type"` is extracted via `settings["type"].(string)` (chainstack.go:204-206) into `ChainstackFilterParams.Type`. This does NOT collide with `UpstreamConfig.Type` (which is the upstream kind — `evm`, `substrate`, etc.) because settings are a separate `VendorSettings` map. However, the name `type` may surprise operators if they confuse vendor settings keys with upstream config fields. | thirdparty/chainstack.go:51-57,189-209,211-230,239-255 |
| onfinality | `apiKey` | yes | — | `https://{netName}.api.onfinality.io/rpc?apikey={apiKey}` (URL-escaped) | thirdparty/onfinality.go:74-96 |
| erpc | `endpoint` | yes | — | target eRPC base; `/{chainId}` is appended to the path | thirdparty/erpc.go:127-130,181-185,234-238 |
| erpc | `secret` | no | `""` | set as `?secret=` query param; overrides an existing `secret` param in the URL | thirdparty/erpc.go:51,132,187-192,240-245 |
| blockpi | `apiKey` | yes | — | `https://{netName}.blockpi.network/v1/rpc/{apiKey}` (URL-escaped); `movement` uses `/rpc/v1/{apiKey}/v1` | thirdparty/blockpi.go:105-132,118-122 |
| ankr | `apiKey` | yes | — | `https://rpc.ankr.com/{netName}/{apiKey}` (URL-escaped) | thirdparty/ankr.go:96-118 |
| routemesh | `apiKey` | yes (SupportsNetwork errors without it) | — | `https://{baseURL}/rpc/{chainId}/{apiKey}` | thirdparty/routemesh.go:52-55,112-115,142-150 |
| routemesh | `baseURL` | no | `lb.routemes.sh` (`DefaultRoutemeshBaseURL`) | host for the URL template | thirdparty/routemesh.go:20,47-50,108-111 |
| blockdaemon | `apiKey` | yes — even when the endpoint is preset (always injected as header) | — | `Authorization: Bearer {apiKey}` header; endpoint `https://svc.blockdaemon.com/{path}` | thirdparty/blockdaemon.go:85-88,104,114-119 |

#### Endpoint URL shorthand (upstream → provider conversion)

Any `upstreams[].endpoint` whose scheme is **not** `http://`, `https://`, `grpc://`, `grpc+bds://` is converted to a ProviderConfig at `ProjectConfig.SetDefaults` and removed from the upstreams list (common/defaults.go:1152-1172, 1193-1241). The `evm+` scheme prefix is stripped (`evm+alchemy://` ≡ `alchemy://`, defaults.go:1206). Settings derivation per scheme (`buildProviderSettings`, common/defaults.go:1243-1425):

| shorthand | derived settings | source |
|---|---|---|
| `alchemy://<apiKey>` | `{apiKey: host}` | defaults.go:1245-1248 |
| `blastapi://<apiKey>` | `{apiKey: host}` | defaults.go:1249-1252 |
| `drpc://<apiKey>` | `{apiKey: host}` | defaults.go:1253-1256 |
| `envio://<rootDomain>` | `{rootDomain: host}` | defaults.go:1257-1260 |
| `etherspot://<apiKey>` | `{apiKey: host}` | defaults.go:1261-1264 |
| `infura://<apiKey>` | `{apiKey: host}` | defaults.go:1265-1268 |
| `llama://<apiKey>` | `{apiKey: host}` | defaults.go:1269-1272 |
| `pimlico://<apiKey>` | `{apiKey: host}` | defaults.go:1273-1276 |
| `thirdweb://<clientId>` | `{clientId: host}` | defaults.go:1277-1280 |
| `dwellir://<apiKey>` | `{apiKey: host}` | defaults.go:1281-1284 |
| `conduit://<apiKey>` | `{apiKey: host}` | defaults.go:1285-1288 |
| `superchain://<spec>` | `{registryUrl: host+path}` | defaults.go:1289-1296 |
| `tenderly://<apiKey>` | `{apiKey: host}` | defaults.go:1297-1300 |
| `quicknode://<apiKey>?tagIds=1,2&tagLabels=a,b` | `{apiKey: host}` + parsed `tagIds` (int or []int) and `tagLabels` (string or []string) | defaults.go:1301-1351 |
| `chainstack://<apiKey>?project=..&region=..` | `{apiKey: host}` + **every** query param copied verbatim (single value → string, repeated → []string) | defaults.go:1352-1372 |
| `onfinality://<apiKey>` | `{apiKey: host}` | defaults.go:1373-1376 |
| `blockpi://<apiKey>` | `{apiKey: host}` | defaults.go:1377-1380 |
| `ankr://<apiKey>` | `{apiKey: host}` | defaults.go:1381-1384 |
| `blockdaemon://<apiKey>` | `{apiKey: host}` | defaults.go:1385-1388 |
| `erpc://<host>[/<path>][?secret=..]` | `{endpoint: "https://"+host+"/"+path}` + `secret` from query if present — note scheme forced to https here (port-based inference only applies to settings-level `erpc://` endpoints) | defaults.go:1389-1404 |
| `repository://<host>/<path>?<query>` | `{repositoryUrl: "https://"+host+"/"+path+"?"+query}` | defaults.go:1405-1408 |
| `routemesh://<host>/rpc/<chainId>/<apiKey>` | `{baseURL: host, apiKey: pathParts[2]}`; path **must** match `/rpc/<chainId>/<apiKey>` else config error | defaults.go:1409-1421 |
| anything else | error `unsupported vendor name in vendor.settings: <scheme>` | defaults.go:1424 |

The conversion copies the whole upstream config (minus `id`/`endpoint`) into `overrides: {"*": ...}` so failsafe/rate-limit/method filters carry over to every generated upstream, and sets `upstreamIdTemplate: "<PROVIDER>-<NETWORK>"` (defaults.go:1212-1235).

#### Implicit default providers

`projects[].providers` defaults to `[]`; if both `providers` and `upstreams` are empty **and** no CLI endpoints were passed, two providers are appended: `{id: "public", vendor: "repository"}` and `{id: "envio", vendor: "envio"}` with empty settings (all defaults), after logging `no providers or upstreams found in project; will use default 'public' endpoints repository` (common/defaults.go:1113-1146; CLI endpoints injection cmd/erpc/main.go:325-336).

**Exact conditions for auto-injection** (all three must be true simultaneously, common/defaults.go:1113-1115):
1. `len(p.Providers) == 0` — providers list is empty (including explicitly set to `[]`).
2. `len(p.Upstreams) == 0` — upstreams list is empty after defaults processing.
3. `opts == nil || len(opts.Endpoints) == 0` — no CLI `--endpoint` flags were supplied.

Setting `providers: []` explicitly in YAML still satisfies condition 1, so if you also have no upstreams and no CLI endpoints, auto-injection fires even on an explicit empty providers list. There is no YAML opt-out short of providing at least one provider or one upstream.

**No customization of auto-injected providers**: the injected `repository` provider uses id `"public"`, vendor `"repository"`, and entirely empty settings — meaning `repositoryUrl` defaults to `https://evm-public-endpoints.erpc.cloud` and `recheckInterval` defaults to `1h`, `autoIgnoreUnsupportedMethods` defaults to `true`, etc. Similarly the injected `envio` provider uses all envio defaults (`rootDomain=rpc.hypersync.xyz`). There is no mechanism to configure these auto-injected instances (e.g. point `repository` at a private mirror or set envio's `apiKey`) without opting out of auto-injection by explicitly listing providers.

### Behaviors & algorithms

**Vendor registry lookup precedence** (thirdparty/vendors_registry.go:45-71):
- `LookupByName`: linear scan, exact `Name()` equality.
- `LookupByUpstream`: if `upstream.vendorName != ""` → exact name match only (nil if the name is unknown — `OwnsUpstream` is NOT consulted); else → first registered vendor whose `OwnsUpstream(cfg)` returns true, in registration order (alchemy first … blockdaemon last; vendors_registry.go:12-33).

**Provider.SupportsNetwork precedence** (thirdparty/provider.go:33-52, pinned by thirdparty/provider_test.go:40-115): `ignoreNetworks` exact-match deny → `onlyNetworks` exact-match allow (else false, vendor never called) → `vendor.SupportsNetwork(ctx, logger, settings, networkId)`.

**Provider.GenerateUpstreamConfigs** (thirdparty/provider.go:54-65): `buildBaseUpstreamConfig(networkId)` → `vendor.GenerateConfigs(ctx, logger, baseCfg, settings)` → `os.ExpandEnv` on each `Endpoint`. Base config build (provider.go:77-138): first wildcard-matching override is `Copy()`-ed (match errors skipped); else `&UpstreamConfig{}` + `SetDefaults(upstreamDefaults)` + `Id=""`; then `VendorName = cfg.Vendor`, `Id = template render`, and for `evm:` networks `Type=evm`, `Evm.ChainId` parsed (ParseInt base 10) and `Evm.SetDefaults(upstreamDefaults.Evm)`.

**Lazy bootstrap orchestration** (upstream/registry.go:155-260, 477-510): per-network `sync.Once` schedules all provider tasks on the app context (survives the triggering request); task body: `SupportsNetwork` — `(false,nil)` → log + return nil (task succeeds, no upstreams); error → task fails (auto-retry); else `GenerateUpstreamConfigs` → `registerUpstreams` which fans out per-upstream tasks named `network/evm:<chainId>/upstream/<id>` (or `upstream/<id>` when no chain id) (registry.go:411-427). Wait loop: minReady=1, timeout 30s capped by ctx deadline (≤0 → 50ms), tick 200ms; with 0 upstreams (on tick or timeout): tasks ongoing → keep waiting / `ErrNetworkInitializing` on timeout; all provider tasks terminal (succeeded/fatal) → 100ms grace re-check then retryable `ErrNetworkNotSupported` if still empty (registry.go:188-293, 529-562).

**RemoteDataCache algorithm** (thirdparty/remote_cache.go:99-267): snapshot = `atomic.Pointer[{values map[string]T, fetchedAt map[string]time.Time}]`. `Lookup(key, interval)` → `(zero,false)` if no snapshot/key; else `(val, time.Since(fetchedAt) < interval)`. `TriggerAsyncRefresh`: mutex guards only the in-flight set (single-flight per key, second caller no-ops); goroutine with `context.WithTimeout(Background, 90s)`; on error keep old snapshot + warn log; on success copy-on-write merge + `Store`. Panic in fetcher recovered + error log. `EnsureFresh` (L252-267) is provided but currently unused — all vendors hand-roll Lookup+Trigger. `ErrRemoteCacheCold = "vendor remote-data cache not yet populated; retry shortly"` (L85) is the retryable cold-start sentinel.

**Initializer auto-retry** (consumes failed provider/upstream tasks): enabled by default, exponential factor 1.5, min delay 3s, max delay 130s (util/initializer.go:179-182).

**Vendor error-mapping hook**: `architecture/evm/error_normalizer.go:20-31` — on any JSON-RPC error body, `getVendorSpecificErrorIfAny` (L878-900) resolves `req.LastUpstream().Vendor()` and calls `GetVendorSpecificErrorIfAny(req, httpResp, jsonRpcResp, details)` *before* generic mapping; non-nil return short-circuits. Per-vendor mappings are listed in the vendor subsections below; vendors returning always-nil: dwellir, envio, etherspot, pimlico, repository, superchain, routemesh, thirdweb (their files' `GetVendorSpecificErrorIfAny`). Several vendors (ankr, blastapi, chainstack, erpc, onfinality, tenderly, blockpi partially, blockdaemon partially) only copy `err.Data` into `details["data"]` and otherwise defer to generic handling.

**Upstream id rendering** (`applyUpstreamIDTemplate`, thirdparty/provider.go:141-162): straight `strings.ReplaceAll`; `<EVM_CHAIN_ID>` becomes `N/A` for non-`evm:` networks.

### Vendor reference (one subsection per vendor)

#### alchemy (thirdparty/alchemy.go)
- **Chain support**: dynamic catalog from `chainsUrl` (default `https://app-api.alchemy.com/trpc/config.getNetworkConfig`, a `var` so tests can swap it; alchemy.go:156-158) via `RemoteDataCache[map[int64]string]` keyed by the URL; **cold-start fallback** to the built-in `defaultAlchemyNetworkSubdomains` map of 134 chains (alchemy.go:17-152, 344-356). Fetch decodes `{result:{data:[{networkChainId,kebabCaseId}]}}`, drops zero/empty entries, then **merges the static defaults in** (API wins on conflict, static fills gaps) (alchemy.go:386-405). Fetch timeouts: 10s request ctx, 30s client (alchemy.go:359-374).
- **GenerateConfigs**: requires `apiKey`, `upstream.evm`, non-zero `chainId` when endpoint empty; URL `https://{subdomain}.g.alchemy.com/v2/{apiKey}`; always stamps `VendorName="alchemy"`; emits a debug log that dereferences `upstream.Evm.ChainId` unconditionally (alchemy.go:215-273).
- **OwnsUpstream**: `alchemy://`/`evm+alchemy://` prefix, `vendorName == "alchemy"`, or endpoint contains `.alchemy.com` / `.alchemyapi.io` (alchemy.go:328-338).
- **Error mapping** (alchemy.go:275-326): code `-32600` + message containing `be authenticated`/`access key` → `ErrEndpointUnauthorized`; codes in ranges `[-32599,-32099]`, `[-32699,-32603]`, `[-32768,-32701]` (as written: `code >= -32099 && code <= -32599 || ...` — note these Go conditions are impossible to satisfy as written since -32099 > -32599; the literal condition `code >= -32099 && code <= -32599` is never true, ditto the other two ranges, so in practice only -32600-auth and code-3 branches fire) → intended `ErrEndpointClientSideException` with `retryableTowardNetwork=false`; code `3` → `ErrEndpointExecutionException` (EVM revert).
- **Tests** (thirdparty/alchemy_test.go): cold-start fallback for SupportsNetwork & GenerateConfigs (L16-63); async refresh promotes API data over fallback and the *snapshot replaces* the static set for chains the API omitted (L65-116); `chainsUrl` override works and static defaults remain merged (L118-158); per-URL cache isolation — custom URL data never bleeds into the default-URL key (L175-207); malformed `chainsUrl` (`not-a-url`, `ftp://host`, `://missing-scheme`) → error containing `invalid chainsUrl` (L209-223).

#### ankr (thirdparty/ankr.go)
- Static map of 47 chains → path names (`eth`, `bsc`, `arbitrum`, …; ankr.go:15-63). SupportsNetwork = map lookup (L77-88).
- URL: `https://rpc.ankr.com/{netName}/{url.QueryEscape(apiKey)}` (L108). Preset endpoint passes through. Requires `upstream.evm` + chainId (L97-103).
- OwnsUpstream: `ankr://`/`evm+ankr://` or contains `rpc.ankr.com` (L138-144). Error hook: copies `err.Data` only (L124-136).

#### blastapi (thirdparty/blastapi.go)
- Static map of 79 chains (L15-95). URL `https://{netName}.blastapi.io/{apiKey}`; Avalanche (`ava-mainnet`/`ava-testnet`) appends `/ext/bc/C/rpc` (L140-144).
- OwnsUpstream: scheme prefixes or `.blastapi.io` (L175-181). Error hook: data passthrough only (L160-173).

#### blockdaemon (thirdparty/blockdaemon.go)
- Static map of 18 chain-id → path suffixes with heterogeneous shapes: `/native`, `/native/http-rpc`, Avalanche `/native/ext/bc/c/eth`, Tron `/native/jsonrpc`, Arbitrum `mainnet-one` (L24-53; pinned by blockdaemon_test.go:61-87). Base `https://svc.blockdaemon.com/{path}` (L104).
- **Auth is header-based**: `apiKey` is required *before* the endpoint check — even a preset endpoint errors without it — and is injected as `Authorization: Bearer <apiKey>` unless an Authorization header already exists (L85-88, 114-119; test L88-97 passes settings with apiKey).
- HTTP 401 → `ErrEndpointUnauthorized` (L135-145). OwnsUpstream: scheme prefixes or `svc.blockdaemon.com` (L150-156; test L100-117).

#### blockpi (thirdparty/blockpi.go)
- Static map of 56 chains (exported `BlockPiNetworkNames`, L15-72). URL `https://{netName}.blockpi.network/v1/rpc/{escapedKey}`; chain `movement` (126) uses `https://movement.blockpi.network/rpc/v1/{escapedKey}/v1` (L118-122).
- Error mapping: lowercase message containing `apikey is on another chain`/`api key is on another chain` → `ErrEndpointUnauthorized` (L150-162). OwnsUpstream: scheme prefixes or `.blockpi.network` (L167-173).

#### chainstack (thirdparty/chainstack.go)
- **Account-level node discovery**: paginated GET `https://api.chainstack.com/v1/nodes/` with `Authorization: Bearer {apiKey}` and optional `project/organization/region/provider/type` query filters, following `next` links (L232-312). Each raw node is decoded individually — malformed nodes are skipped with a debug log, and nodes lacking id or `https_endpoint` are dropped (L289-302; resilient-decode pinned by chainstack_test.go:126-157). Chain ids are then discovered by POSTing `eth_chainId` to every running node (concurrency-limited by a weighted semaphore of 10, 10s client timeout, hex-parsed; failures collected and logged as a warning without failing the refresh) (L314-406).
- **Cache key includes filters**: `{apiKey}[_p:project][_o:org][_r:region][_pr:provider][_t:type]` (L211-230; pinned by chainstack_test.go:214-258) — two providers with the same key but different filters get separate snapshots.
- SupportsNetwork: non-EVM → false; missing apiKey → `(false, nil)`; cold cache → `ErrRemoteCacheCold`; else true iff some node has matching ChainID and `status=="running"` (L73-102).
- **GenerateConfigs fan-out**: one upstream per matching running node with non-empty endpoint; endpoint = `https_endpoint + "/" + auth_key`; id = `{upstream.Id}-{nodeID}` or `chainstack-{chainId}-{nodeID}` (L154-183). Preset endpoint short-circuits with no API calls (L184-186; test L62-79). Default `recheckInterval` 1h (L59).
- OwnsUpstream: scheme prefixes or `.core.chainstack.com` (L423-429). Error hook: data passthrough only (L408-421).

#### conduit (thirdparty/conduit.go)
- Dynamic-only discovery from `networksUrl` (default `https://api.conduit.xyz/public/network/all`, L16) decoding `{endpoints:[{id,name,chainId,httpEndpoint,wsEndpoint}]}`; entries with unparseable/≤0 chainId or empty httpEndpoint dropped (L224-272). No static fallback → cold start returns `ErrRemoteCacheCold` from both SupportsNetwork and GenerateConfigs (L68-74, 136-139).
- GenerateConfigs: preset endpoint bypasses everything (L104-106); else requires evm.chainId and `apiKey`; endpoint = `{httpEndpoint}/{apiKey}`; copies the base config, sets `VendorName` (L141-158).
- OwnsUpstream: scheme prefixes or `vendorName=="conduit"` — **no domain heuristic** (chains live on customer domains) (L212-222).
- Error mapping (L161-210): `-32600` + `be authenticated`/`access key`/`api key` → unauthorized; message `limit exceeded`/`capacity limit` → `ErrEndpointCapacityExceeded`; code in `[-32099,-32000]` → `ErrEndpointServerSideException` (with HTTP status). (Note: the code writes `code >= -32000 && code <= -32099`, which is never true as written — same impossible-range pattern as alchemy.)

#### drpc (thirdparty/drpc.go)
- Dynamic catalog from `chainsUrl` (default `https://lb.drpc.org/networks`, a swappable `var`, L172-173) with a 150-chain static fallback map (L19-170). Fetch filters to `blockchain_type=="eth" && api_type=="jsonrpc" && has_premium && chain_id != ""`; chain ids are hex-parsed (`0x` optional); when one chain id maps to several names the highest `priority` wins (L390-417).
- **Forces `autoIgnoreUnsupportedMethods = false`** on every config it touches (even preset endpoints) because dRPC sometimes routes to nodes that lack a method transiently — a missing-method response must stay retryable (L252-255).
- URL `https://lb.drpc.org/ogrpc?network={name}&dkey={apiKey}` (L292). Also: if it owns the upstream and `Type` is empty, sets `Type=evm` (L301-303). `chainsUrl` validated structurally like alchemy's (L222-224, 276-278).
- OwnsUpstream: scheme prefixes or `.drpc.org` (L352-358).
- Error mapping (L308-350): message contains `token is invalid` → unauthorized; `ChainException: Unexpected error (code=40000)` or `invalid block range` → `ErrEndpointMissingData` (carries `req.LastUpstream()`).
- Tests (thirdparty/drpc_test.go): cold-start fallback (L16-60), async refresh promotes custom chain (L62-101), invalid `chainsUrl` errors (L103-117).

#### dwellir (thirdparty/dwellir.go)
- Static map of 70 chain → subdomain prefixes (e.g. `api-ethereum-mainnet.n`, L15-86); URL `https://{subdomain}.dwellir.com/{apiKey}`; Avalanche subdomain appends `/ext/bc/C/rpc` (L148-152).
- SupportsNetwork quirk: an unparseable numeric part returns `(false, nil)` rather than an error (L109-113) — unlike every other static-map vendor which propagates the parse error.
- Preset endpoint bypasses generation (L125-127). OwnsUpstream: only `dwellir://` prefix (NOT `evm+dwellir://`) or `.dwellir.com` substring (L173-180) — the `evm+` form still works because shorthand conversion handles it first (defaults.go:1281-1284). Error hook: always nil (L166-170).

#### envio (thirdparty/envio.go) — HyperRPC
- **Method allow-list injected**: when not user-set, `ignoreMethods: ["*"]` plus `allowMethods` of exactly 14 read-oriented methods: `eth_chainId, eth_blockNumber, eth_getBlockByNumber, eth_getBlockByHash, eth_getTransactionByHash, eth_getTransactionByBlockHashAndIndex, eth_getTransactionByBlockNumberAndIndex, eth_getTransactionReceipt, eth_getBlockReceipts, eth_getLogs, eth_getFilterLogs, eth_getFilterChanges, eth_uninstallFilter, eth_newFilter` (L174-194). Applies even to preset endpoints (and to statically-configured envio upstreams via the NewUpstream vendor pass).
- SupportsNetwork: 61 known chain ids short-circuit true (L22-84, 111-113); unknown chains are probed **live**: build `https://{chainId}.{rootDomain}[/{apiKey}]`, send `eth_chainId` through a cached headless `HttpJsonRpcClient` (keyed by chainId in a `sync.Map`, built around `phonyUpstream{id:"temp-envio-<chainId>"}`), 10s timeout, and compare the hex result to the requested chain id (L101-167, 242-258). A TLS error containing `failed to verify certificate` is treated as *unsupported* (false, nil) — an artifact of Envio's k8s load-balancer serving unknown subdomains with a bad cert (L139-141).
- GenerateConfigs: URL as above; requires `evm.chainId` (note: dereferences `upstream.Evm` without nil check, L202).
- OwnsUpstream: endpoint *prefix* `envio`/`evm+envio` (covers `envio://`) or contains `envio.dev`/`hypersync.xyz` (L221-226). Error hook: always nil (L217-219).

#### erpc (thirdparty/erpc.go) — chain to another eRPC
- Settings: `endpoint` (required), `secret` (optional). `parseEndpointURL(endpoint, secret, chainId)` (L171-248): `http(s)://` endpoints are parsed as-is; `erpc://`-prefixed or bare host endpoints get **port-based scheme inference**: port 443 → https, 80 → http, none → https, any other port → http. `/{chainId}` is appended to the path (after trimming a trailing slash); a non-empty `secret` is set as `?secret=` (overwriting one already in the URL). Pinned exhaustively by erpc_test.go:9-160 (incl. query-param preservation and `secret=url_secret` being replaced by the parameter).
- SupportsNetwork: requires `endpoint` setting (else false,nil); probes live `eth_chainId` via a cached headless client keyed `"{url}-{chainId}"` (L35-98, 250-265).
- GenerateConfigs: preset `erpc://`/`evm+erpc://` endpoint is itself converted via `parseEndpointURL` (uses `upstream.Evm.ChainId` — nil `Evm` would panic); preset http(s) endpoint passes through; otherwise settings endpoint + secret are used (L100-143).
- OwnsUpstream: scheme prefixes, `vendorName=="erpc"`, or `.erpc.cloud` (L159-169). Error hook: data passthrough only (L145-157).

#### etherspot (thirdparty/etherspot.go) — ERC-4337 bundler (Skandha)
- **Method allow-list injected**: `ignoreMethods: ["*"]` + `allowMethods: [skandha_config, skandha_feeHistory, skandha_getGasPrice, eth_getUserOperationReceipt, eth_getUserOperationByHash, eth_sendUserOperation]` when unset (L81-93).
- Chains: 15 mainnets (chainId→name) + 11 testnets (set) (L15-45). URL shapes differ: mainnet `https://{networkName}-bundler.etherspot.io/`, testnet `https://testnet-rpc.etherspot.io/v1/{chainId}`; `?apikey={apiKey}` appended unless apiKey is empty or the literal `"public"` (L126-146). Note: a chain in neither map yields an empty URL string (generateUrl has no else-error; SupportsNetwork gates normal flows).
- OwnsUpstream: prefix `etherspot`/`evm+etherspot` or contains `etherspot.io` (L120-124). Error hook: always nil (L116-118).

#### infura (thirdparty/infura.go)
- Static map of 31 chains (L15-47). URL `https://{netName}.infura.io/v3/{apiKey}` (L89). Contains a **dead branch**: the `ava-mainnet`/`ava-testnet` `/ext/bc/C/rpc` special case can never trigger because the map uses `avalanche-mainnet`/`avalanche-fuji` (L17-18, 90-93).
- Error mapping (L108-156): `-32600` + `be authenticated`/`access key` → unauthorized; `-32001`/`-32004` → `ErrEndpointUnsupported`; `-32005` → `ErrEndpointCapacityExceeded`.
- OwnsUpstream: scheme prefixes or `.infura.io` (L158-163).

#### llama (thirdparty/llama.go)
- Static map of just 6 chains (eth, arbitrum, base, binance, optimism, polygon; L14-21). URL `https://{netName}.llamarpc.com/{apiKey}` (L62).
- OwnsUpstream matches **only** `.llamarpc.com` — no `llama://` scheme check (L95-97); the shorthand still works because conversion happens before vendor lookup.
- Error mapping: message containing `code: 1015` (Cloudflare rate-limit) → `ErrEndpointCapacityExceeded` (L85-89).

#### onfinality (thirdparty/onfinality.go)
- Static map of 25 chains (L15-41). URL `https://{netName}.api.onfinality.io/rpc?apikey={escapedKey}` — apiKey as query param (L86).
- OwnsUpstream: scheme prefixes or `.api.onfinality.io` (L116-122). Error hook: data passthrough only (L102-114).

#### pimlico (thirdparty/pimlico.go) — ERC-4337 bundler/paymaster
- **Method allow-list injected** *after* endpoint generation: `ignoreMethods: ["*"]` + `allowMethods: [eth_sendUserOperation, eth_estimateUserOperationGas, eth_getUserOperationReceipt, eth_getUserOperationByHash, eth_supportedEntryPoints, pimlico_sendCompressedUserOperation, pimlico_getUserOperationGasPrice, pimlico_getUserOperationStatus, pm_sponsorUserOperation, pm_getPaymasterData, pm_getPaymasterStubData]` (L191-208).
- SupportsNetwork: 67-chain static set short-circuits true (L21-89); unknown chains require `apiKey` (missing → **error**, message says set `'public'` for the public endpoint) and are probed live via `eth_chainId` (headless client cached per chainId; 10s timeout with cause) (L106-167).
- URL: apiKey `"public"` → `https://public.pimlico.io/v2/{chainId}/rpc`; else `https://api.pimlico.io/v2/{chainId}/rpc?apikey={apiKey}` (L223-235).
- OwnsUpstream: prefix `pimlico`/`evm+pimlico` or contains `pimlico.io` (L217-221). Error hook: always nil (L213-215).

#### quicknode (thirdparty/quicknode.go)
- **Account endpoint discovery**: paginated GET `https://api.quicknode.com/v0/endpoints?limit=100&offset=N` with header `x-api-key`, optional `tag_ids`/`tag_labels` comma-joined filters; endpoints without `http_url` dropped; loop ends when a page returns fewer than 100 (L207-284). A response-level `error` field aborts the refresh (L264-266). Chain ids discovered by `eth_chainId` probe per endpoint (semaphore 10, 10s timeout; partial failures logged, refresh continues) (L286-378).
- **Cache key is the apiKey only** (`v.cache.Lookup(apiKey, ...)`, L184-205) — unlike chainstack, the tag filters are NOT part of the key, so two providers sharing an apiKey with different `tagIds`/`tagLabels` share one snapshot (whoever refreshes first defines the contents).
- SupportsNetwork: missing apiKey → `(false,nil)`; cold cache → `ErrRemoteCacheCold`; else true iff some endpoint has the chain id + non-empty URL (L99-129). Default `recheckInterval` 1h (L44).
- **GenerateConfigs fan-out**: one upstream per matching endpoint; id `{upstream.Id}-{endpointID}` or `quicknode-{chainId}-{endpointID}`; endpoint = discovery `http_url` verbatim (L134-179). Preset endpoint passes through.
- `tagIds`/`tagLabels` accept scalar, typed slice, or `[]interface{}` (L56-92; pinned by quicknode_test.go:10-41); the URL shorthand parses `?tagIds=1,2&tagLabels=a,b` into the same shapes (defaults.go:1306-1349).
- Error mapping (L380-444) — note it builds a fresh local `details` map shadowing the parameter: `-32614` or (`eth_getLogs` + message `limited to`) → `ErrEndpointRequestTooLarge(EvmBlockRangeTooLarge)`; `-32009`/`-32007` → capacity; `-32612`/`-32613` → `ErrEndpointUnsupported` (constructed with the `JsonRpcErrorCapacityExceeded` normalized code, L407); message `failed to parse` → client-side, not retryable toward network; `-32010` (tx cost exceeds gas limit) → client-side but network-retryable (per-client gas caps differ); `-32602` + `cannot unmarshal hex string` → invalid-argument client-side, not retryable; message `UNAUTHORIZED` → unauthorized; code `3` → EVM revert.
- OwnsUpstream: scheme prefixes or `.quiknode.pro` (L446-452).

#### repository (thirdparty/repository.go) — public endpoints catalog
- Fetches `repositoryUrl` (default `https://evm-public-endpoints.erpc.cloud`) returning `{"<chainIdString>": {"endpoints": ["https://...", ...]}}`; non-integer keys skipped (L182-232). No static fallback → `ErrRemoteCacheCold` on cold start (L61-64, 112-115). Default recheck 1h (L18).
- **GenerateConfigs fan-out**: one upstream per endpoint that starts with `http` (case-insensitive; wss/ws skipped), id = `{providerRenderedId}-{RedactEndpoint(ep)}` where RedactEndpoint yields e.g. `https#redacted=ab12c` (scheme + 5-char sha256 prefix; util/redact.go:10-36). Each upstream gets deep copies of `Evm`, `JsonRpc`, `Failsafe` (per-element `Copy()`), `RateLimitAutoTune`, `Tags`, and inherits `IgnoreMethods/AllowMethods/RateLimitBudget` (L121-166).
- **Defaults `autoIgnoreUnsupportedMethods: true`** when unset — public RPCs are wildly inconsistent in method support (L89-92).
- SupportsNetwork: true iff the chain id is present with ≥1 endpoint (L41-67). Requires `upstream.evm.chainId` for generation (L94-100).
- OwnsUpstream: only the `repository://`/`evm+repository://` schemes (L177-180) — generated upstreams' endpoints are arbitrary third-party hosts, so no domain matching is possible; vendor attribution for them relies on the `VendorName` stamped by the provider flow.
- This is half of the implicit default provider pair (`id: public`; common/defaults.go:1126-1133).

#### routemesh (thirdparty/routemesh.go)
- URL `https://{baseURL}/rpc/{chainId}/{apiKey}` with `baseURL` default `lb.routemes.sh` (L20, 142-150). No chain catalog at all: SupportsNetwork always probes live `eth_chainId` (headless client cached per `"{url}-{chainId}"`; 10s timeout) and errors if `apiKey` missing (L37-100, 152-167).
- Shorthand `routemesh://<host>/rpc/<chainId>/<apiKey>` requires that exact path shape (`pathParts[0]=="rpc"`, ≥3 segments) else a config-load error; apiKey is segment index 2 (defaults.go:1409-1421).
- OwnsUpstream: `vendorName=="routemesh"` or endpoint contains `routemes.sh` — no scheme-prefix check (L135-140). Error hook: always nil (L131-133).

#### superchain (thirdparty/superchain.go) — OP Superchain registry
- Registry spec parser `parseSuperchainSpec` (L26-88) accepts: `github.com/{org}/{repo}` (branch `main`, file `chainList.json` defaults), `github.com/{org}/{repo}/{branch}`, `github.com/{org}/{repo}/{branch}/{path/to/file.json}`, GitHub UI URLs with `/blob/` or `/tree/` (segment stripped), `github.com/{org}/{repo}/{file}.json` (implicit main), full `http(s)://` URLs passed through, and bare `domain/path` (https:// prepended). GitHub specs are rewritten to `https://raw.githubusercontent.com/{org}/{repo}/{branch}/{file}`.
- Registry JSON is an array of `{chainId, rpc:[...]}`; entries with chainId ≤ 0 or empty rpc dropped (L266-312). No static fallback → `ErrRemoteCacheCold` cold start (L137-140, 210-213). Default registryUrl = the official `ethereum-optimism/superchain-registry` chainList.json (L16); default recheck 24h (L17).
- **GenerateConfigs fan-out**: one upstream per rpc URL with id suffix `-{index}` (`{id}-0`, `{id}-1`, … or `superchain-{chainId}-{i}`), `VendorName="superchain"` (L220-236). Special endpoint handling: an `upstream.Endpoint` of `superchain://<spec>` (or `evm+`) is parsed into `settings["registryUrl"]` and the endpoint cleared so fan-out happens; any other non-empty endpoint passes through (L168-184).
- OwnsUpstream: scheme prefixes or `vendorName=="superchain"`; no domain matching (L254-264). Error hook: always nil (L250-252).

#### tenderly (thirdparty/tenderly.go)
- Catalog from hardcoded `https://api.tenderly.co/api/v1/supported-networks` (const — **not** settings-overridable; L35) decoding `chain_id` + `network_slugs`; slug preference `node_rpc_slug` > `vnet_rpc_slug` > `explorer_slug`; entries with no usable slug or unparseable chain id skipped (L162-218). No static fallback → `ErrRemoteCacheCold` (L61-64, 111-114). Default recheck 24h (L34).
- URL `https://{slug}.gateway.tenderly.co/{apiKey}` (L121); stamps `VendorName="tenderly"` always (L131).
- OwnsUpstream: scheme prefixes, `vendorName=="tenderly"`, or `.gateway.tenderly.co` (L150-160). Error hook: data passthrough only (L136-148).

#### thirdweb (thirdparty/thirdweb.go)
- URL `https://{chainId}.rpc.thirdweb.com/{clientId}` (L125-132). No chain catalog: SupportsNetwork always probes live `eth_chainId` (headless client cached per chainId; 10s timeout); missing `clientId` → error (L35-91). (Chain id parsed via `networkId[4:]`, equivalent to the TrimPrefix used elsewhere.)
- OwnsUpstream: scheme prefixes or `.thirdweb.com` (L117-123). Error hook: always nil (L113-115).

#### phony upstream (testing/probing stub) (thirdparty/phony.go)
- `phonyUpstream` embeds `common.Upstream` and implements only `Id()`, `Name()` (both return the id), `Type()` (always `UpstreamTypeEvm`), `Vendor()` (nil), `VendorSettings()` (nil) (L5-31). Any other interface method panics (nil embedded interface).
- Used to satisfy `clients.NewGenericHttpJsonRpcClient`'s upstream parameter when vendors need a throwaway client for live `eth_chainId` probes before any real upstream exists: ids `temp-envio-<chainId>` (envio.go:249), `temp-erpc-<chainId>` (erpc.go:257), `temp-pimlico-<chainId>` (pimlico.go:244), `temp-routemesh-<chainId>` (routemesh.go:159), `temp-thirdweb-<chainId>` (thirdweb.go:141).
- The unit-test counterpart is `MockVendor` in thirdparty/provider_test.go:14-38 (testify mock implementing `common.Vendor`); a separate general-purpose `FakeUpstream` lives in common/upstream_fake.go (different subsystem).

### Observability

- **No provider/vendor-specific Prometheus metrics exist.** The `vendor` label carried by virtually every upstream-level metric in telemetry/metrics.go (e.g. `erpc_upstream_request_total` and siblings, labels `project, vendor, network, upstream, ...`; telemetry/metrics.go:23,29,35,41,47,53-71,168,310-328,370-388,501-507) is populated from `Upstream.VendorName()` — the attached vendor's `Name()`, the config's `vendorName`, or the guessed `unknown-<rootDomain>` (upstream/upstream.go:275-286, 202-208, 1471-1503).
- **Trace spans**: `UpstreamsRegistry.buildProviderBootstrapTask` detail span per provider task (upstream/registry.go:485); `UpstreamsRegistry.buildUpstreamBootstrapTask` for each registered upstream (registry.go:431).
- **Initializer task names** (visible in initializer status / logs): `network/<networkId>/provider/<providerId>` (registry.go:481) and `network/<networkId>/upstream/<upstreamId>` / `upstream/<upstreamId>` (registry.go:423-427).
- **Notable log messages**:
  - warn `no providers or upstreams found in project; will use default 'public' endpoints repository` (common/defaults.go:1125)
  - debug `attempting to create upstream(s) from provider` / debug `provider does not support network; skipping upstream creation` / info `registering N upstream(s) from provider` (upstream/registry.go:489-505)
  - error `failed to execute provider bootstrap tasks` / info `provider bootstrap tasks executed successfully` (upstream/registry.go:174-184)
  - warn `vendor remote-data refresh failed; keeping previous snapshot` with `vendor` + `cacheKey` fields; error `panic recovered during vendor remote-data async refresh` (thirdparty/remote_cache.go:196-218)
  - warn `some quicknode chain ID fetches failed; continuing with available data` (quicknode.go:196) and chainstack equivalent (chainstack.go:118); warn `failed to fetch chain IDs for some QuickNode endpoints` (quicknode.go:374) / `...Chainstack nodes` (chainstack.go:402)
  - info `successfully fetched dRPC network names` with count+url (drpc.go:419)
  - debug `generated upstream from alchemy provider` (alchemy.go:268-270), `generated upstream from conduit provider` (conduit.go:153-156), `generated upstreams from repository provider` (repository.go:168), `generated upstreams from superchain provider` (superchain.go:238-245)

### Edge cases & gotchas

1. **`recheckInterval` from YAML is silently ignored** — every vendor reads it with `.(time.Duration)` (e.g. repository.go:56-59, quicknode.go:114-117, alchemy.go:205-208); YAML scalars decode to string/int in `map[string]interface{}` so the assertion fails and the default applies. Only programmatic configs can set it.
2. **Cold-start contract differs by vendor**: alchemy/drpc fall back to built-in maps and return `(supported?, nil)` (alchemy_test.go:16-39, drpc_test.go:16-35); conduit/superchain/tenderly/quicknode/chainstack/repository return `ErrRemoteCacheCold` (`"vendor remote-data cache not yet populated; retry shortly"`) which the initializer retries with 3s→130s backoff (remote_cache.go:85; util/initializer.go:179-182; chainstack_test.go:26-44,87-99).
3. **After a successful remote fetch, alchemy/drpc snapshots replace the fallback** for chains the API omitted: alchemy merges static defaults into fetched data (alchemy.go:398-403) so default chains survive, but a custom `chainsUrl` that returns a partial list controls supported-set semantics (alchemy_test.go:102-115 commentary); drpc does **no** merge — fetched data fully replaces the 150-chain fallback (drpc.go:360-422).
4. **Missing-apiKey behavior is inconsistent**: pimlico and routemesh `SupportsNetwork` return an *error* (pimlico.go:120-123, routemesh.go:52-55) so the provider task fails and retries forever; quicknode and chainstack return `(false, nil)` and are skipped silently (quicknode.go:109-112, chainstack.go:83-86); thirdweb errors on missing `clientId` (thirdweb.go:45-48).
5. **blockdaemon requires `apiKey` even with a preset endpoint** (it always injects the Bearer header; blockdaemon.go:85-88). Consequence: a *statically-configured* upstream pointing at `svc.blockdaemon.com` fails at `NewUpstream`, because the vendor is attached via `OwnsUpstream` and `GenerateConfigs` is called with `settings=nil` (upstream/upstream.go:171,191-200) — blockdaemon endpoints only work through the provider path.
6. **Impossible range conditions in alchemy/conduit error mapping**: alchemy's `code >= -32099 && code <= -32599` (and two sibling ranges, alchemy.go:298) and conduit's `code >= -32000 && code <= -32099` (conduit.go:194) can never be true for any integer — these intended client-side/server-side classifications are dead code; only the message-match and code-3 branches actually fire.
7. **infura's Avalanche `/ext/bc/C/rpc` branch is unreachable** — the map values are `avalanche-mainnet`/`avalanche-fuji` but the branch tests `ava-mainnet`/`ava-testnet` (infura.go:17-18,90-93). blastapi/dwellir implement the same special-case correctly (blastapi.go:141-144, dwellir.go:150-152).
8. **`os.ExpandEnv` runs on every provider-generated endpoint** (provider.go:63,67-71): a literal `$` in an apiKey/override endpoint is substituted from the process environment (often to empty). Static upstreams do not get this treatment.
9. **`overrides` first-match is map-iteration order**: with multiple patterns matching one network (e.g. `"*"` and `"evm:1*"`), which override wins is nondeterministic across restarts (provider.go:81-91 iterates a Go map). Pattern-match errors are silently skipped (provider.go:83-86).
10. **`LookupByUpstream` trusts an explicit `vendorName` totally** — if it names a non-existent vendor, the result is nil (no `OwnsUpstream` fallback), so vendor error-mapping silently disappears (vendors_registry.go:54-62). Registration order decides ties for `OwnsUpstream` matching (vendors_registry.go:12-33).
11. **quicknode cache key omits tag filters** (quicknode.go:185-188) while chainstack includes them (chainstack.go:211-230; chainstack_test.go:214-258): two quicknode providers with the same apiKey but different `tagIds`/`tagLabels` share one endpoint snapshot — the first-triggered refresh's filters define what both see.
12. **Per-URL cache isolation works** for catalog vendors keyed by URL: alchemy data fetched for a custom `chainsUrl` never leaks into the default-URL key (alchemy_test.go:175-207).
13. **Shorthand `erpc://host:8080` (upstream endpoint) forces https** because `buildProviderSettings` hardcodes `https://` (defaults.go:1391), whereas the same string in `providers[].settings.endpoint` gets port-based inference (8080 → http; erpc.go:213-228; erpc_test.go:50-57). Use explicit provider settings for non-TLS targets.
14. **`onlyNetworks`/`ignoreNetworks` are exact string matches**, not wildcards (provider.go:34-49), and both must validate as `evm:<positiveInt>` (validation.go:800-813, network.go:39-49); they are mutually exclusive (validation.go:790-792). `ignoreNetworks` is dropped from `MarshalJSON` output (config.go:679-688) though present in YAML marshal.
15. **dRPC upstreams can't auto-ignore unsupported methods** — the vendor pins `AutoIgnoreUnsupportedMethods=false` on every config (even preset/static ones) because dRPC's routing makes missing-method errors transient (drpc.go:252-255); repository does the opposite, defaulting it to `true` (repository.go:89-92).
16. **Live-probe vendors dial out during routing decisions**: envio/erpc/pimlico/routemesh/thirdweb `SupportsNetwork` performs a real `eth_chainId` HTTP call (10s timeout) on the provider bootstrap path — only the catalog-based vendors are fully lock-free/non-blocking there; the probe clients are cached forever in `sync.Map`s (envio.go:242-258 keyed by chainId; erpc.go:250-265 and routemesh.go:152-167 keyed by URL+chainId).
17. **Repository upstream ids leak no secrets**: generated ids embed `RedactEndpoint(ep)` = scheme + 5-hex-char sha256 (`https#redacted=ab12c`), so dashboards can distinguish public endpoints without exposing them (repository.go:153; util/redact.go:10-36).
18. **PrepareUpstreamsForNetwork can succeed with a subset**: it waits for only one ready upstream (minReady=1) and returns success on timeout if *any* upstream registered, so slow providers contribute upstreams later without blocking first requests (upstream/registry.go:188-226). Provider tasks all terminal + zero upstreams → 100ms grace re-check → retryable `ErrNetworkNotSupported`; tasks still ongoing → `ErrNetworkInitializing` (registry.go:227-293).
19. **Chainstack node decode is intentionally lenient**: nodes are decoded one-by-one from `results` raw messages; type mismatches in unrelated fields (e.g. numeric `version`) don't poison the page — the bad node is skipped (chainstack.go:289-302; chainstack_test.go:126-157).
20. **Provider ids must be unique only by convention** — `NewProvidersRegistry` performs no duplicate-id check (providers_registry.go:15-34); two providers with the same vendor and default id (`p.Id = p.Vendor`, defaults.go:1474-1476) both run and may produce upstream-id collisions that the upstream registry then deduplicates by id (registry.go:436-452 reuses an existing upstream with the same id).
21. **OP-stack "sender is over rate limit" is the only capacity error with `retryableTowardNetwork=false`**: the generic error normalizer matches the message `"sender is over rate limit"` (architecture/evm/error_normalizer.go:L125-L139) and produces `ErrEndpointCapacityExceeded` with `WithRetryableTowardNetwork(false)`. This is unique: every other capacity/rate-limit error in the normalizer leaves `retryableTowardNetwork=true` (meaning eRPC will try another upstream). The reason for N:no here is architectural — all OP-stack providers (Alchemy, Infura, Chainstack, public RPCs, etc.) are proxies in front of the **same single OP sequencer**. If the sequencer has rate-limited a sender address, retrying on a different RPC provider for the same network will hit the same limit — the error is sender-level, not upstream-level. The practical effect: when an OP-stack transaction submission gets this error, eRPC immediately returns it to the caller instead of burning retries across every available upstream. Operators on OP-stack chains (Optimism, Base, Zora, etc.) should expect this error to surface to clients rather than being silently retried. Note: this detection is in the **generic** normalizer path (not a vendor-specific hook), so it applies regardless of which vendor is used.

22. **`"unsupported method"` detection is intentionally ordered AFTER `"not found"` rules in the generic normalizer — a Tenderly-specific constraint**: the error normalizer's "Unsupported" block (architecture/evm/error_normalizer.go:L452-L477) checks for messages like `"not supported"`, `"Unsupported method"`, `"method is not whitelisted"`, etc. A code comment at L452-L453 explicitly states: *"do not move this check above 'Not found' errors, as we want to avoid premature detection when message is only 'not found' (e.g. from Tenderly)"*. Tenderly returns bare `"not found"` JSON-RPC error messages for some missing-data conditions; if the "unsupported" block ran first, `strings.Contains(msg, "not supported")` would match that string and misclassify a missing-data error as `ErrEndpointUnsupported`, causing the method to be permanently ignored. The "Not found / disabled" block (L394-L446) fires first: it matches `"not found"` and then further discriminates by whether the message also contains method/module keywords (→ `ErrEndpointUnsupported`) or data/block/header keywords (→ `ErrEndpointMissingData`) or neither (→ retryable `ErrEndpointClientSideException`). Tenderly's bare `"not found"` falls into the last bucket, making it retryable — the correct behavior since the data may be present on another upstream. **Impact for Tenderly operators**: a Tenderly-backed network returning `"not found"` on missing-data will produce `ErrEndpointClientSideException` (retryable toward other upstreams), not `ErrEndpointMissingData`. If Tenderly is the only upstream, the request will ultimately fail with `ErrUpstreamsExhausted` rather than a clean `ErrEndpointMissingData`, which affects which HTTP status code the client sees.

### Source map

- `thirdparty/vendors_registry.go` — global registry; canonical 22-vendor list; LookupByName / LookupByUpstream / SupportedVendors.
- `thirdparty/providers_registry.go` — per-project ProviderConfig → Provider binding; unknown-vendor startup error.
- `thirdparty/provider.go` — Provider wrapper: only/ignore network gating, override selection, id templating, evm chain parsing, env expansion.
- `thirdparty/vendor_utils.go` — `validateChainsURL` structural check (http/https + host) for alchemy/drpc `chainsUrl`.
- `thirdparty/remote_cache.go` — generic lock-free `RemoteDataCache[T]`; request-path safety rule doc; `ErrRemoteCacheCold`.
- `thirdparty/phony.go` — minimal `common.Upstream` stub for headless probe clients.
- `thirdparty/alchemy.go` / `drpc.go` — remote catalog + static fallback vendors (134 / 150 fallback chains).
- `thirdparty/conduit.go`, `superchain.go`, `tenderly.go`, `repository.go` — remote-catalog-only vendors (cold start = retryable).
- `thirdparty/quicknode.go`, `chainstack.go` — account-level endpoint/node discovery with `eth_chainId` enrichment and multi-upstream fan-out.
- `thirdparty/ankr.go`, `blastapi.go`, `blockdaemon.go`, `blockpi.go`, `dwellir.go`, `infura.go`, `llama.go`, `onfinality.go` — static chain-map vendors.
- `thirdparty/envio.go`, `etherspot.go`, `pimlico.go` — method-allowlist vendors (HyperRPC reads / 4337 bundler methods); envio+pimlico also live-probe.
- `thirdparty/erpc.go`, `routemesh.go`, `thirdweb.go` — probe-based vendors (no catalog).
- `thirdparty/*_test.go` (alchemy, drpc, blockdaemon, chainstack, erpc, provider, quicknode) — pin cold-start, fan-out, URL-building, and filter-extraction behaviors.
- `common/vendors.go` — `Vendor` interface definition.
- `common/config.go:667-700` — `VendorSettings`, `ProviderConfig`, secret-redacting marshalers.
- `common/defaults.go:1102-1241,1243-1425,1473-1489` — default providers, shorthand upstream→provider conversion, `buildProviderSettings`, ProviderConfig defaults.
- `common/validation.go:780-815` — ProviderConfig validation rules.
- `upstream/registry.go:155-260,411-510,529-562` — lazy per-network provider bootstrap, task naming, readiness wait.
- `upstream/upstream.go:171-222,275-286,1471-1503` — vendor attach for static upstreams, settings-nil GenerateConfigs pass, VendorName/guessVendorName.
- `architecture/evm/error_normalizer.go:20-31,125-139,394-477,878-900` — vendor-specific error hook wiring; OP-stack sequencer rate-limit with `retryableTowardNetwork=false` (L125-139); "not found" / "unsupported" ordering constraint that is Tenderly-specific (L394-477).
- `util/initializer.go` — bootstrap task engine + auto-retry parameters; `util/ids.go` — shorthand id counters; `util/redact.go` — endpoint redaction for repository upstream ids; `util/schemes.go` — native-protocol detection used by conversion.
- `erpc/erpc.go:67`, `erpc/projects_registry.go:146-154` — registry construction at startup; `cmd/erpc/main.go:325-336` — CLI endpoints that suppress the default providers.
