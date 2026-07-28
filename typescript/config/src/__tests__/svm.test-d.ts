/**
 * Compile-time tests for `@erpc-cloud/config` SVM (Solana) typings.
 *
 * Run via:
 *   pnpm --filter @erpc-cloud/config test
 *
 * which invokes `tsc --noEmit` against this file. Positive cases must
 * compile clean; negative cases are marked with `@ts-expect-error` so an
 * accidental relaxation of the public types fails the build.
 *
 * Every symbol below is imported from the package root — the same specifier
 * a published-package consumer writes. Deep imports (`../generated`) would
 * pass while the package entry point stayed EVM-only, which is exactly the
 * hole this file exists to close.
 */

import {
  createConfig,
  ArchitectureSvm,
  UpstreamTypeSvm,
  type NetworkArchitecture,
  type UpstreamType,
  type SvmNetworkConfig,
  type SvmUpstreamConfig,
  type SvmNetworkConfigForDefaults,
} from "../index";

/* ───────────────────── 1. Realistic SVM config compiles ───────────────── */

const _realisticSvmConfig = createConfig({
  database: {
    svmJsonRpcCache: {
      connectors: [
        { id: "memory", driver: "memory", memory: { maxItems: 10_000, maxTotalSize: "128mb" } },
      ],
      policies: [
        // Solana chain: `chain` is empty/"solana", so the derived network id
        // keeps the pre-multi-chain "svm:<cluster>" form.
        { connector: "memory", network: "svm:mainnet-beta", method: "getBlock", ttl: "5m" },
        // Any other chain derives "svm:<chain>:<cluster>".
        { connector: "memory", network: "svm:fogo:mainnet", method: "getBlock", ttl: "5m" },
      ],
    },
  },
  projects: [
    {
      id: "solana",
      networkDefaults: {
        // `cluster` is omitted from the defaults type (it is per-network),
        // but every other SVM field is settable here — including the
        // enforcement switch, whose explicit `false` must survive the merge.
        svm: { commitment: "confirmed", enforceBlockAvailability: false },
      },
      upstreams: [
        {
          id: "helius-mainnet",
          type: "svm",
          endpoint: "https://mainnet.helius-rpc.com/?api-key=placeholder",
          svm: { cluster: "mainnet-beta" },
        },
        {
          id: "fogo-mainnet",
          type: "svm",
          endpoint: "https://rpc.fogo.io",
          tags: ["tier:fallback"],
          // Non-Solana SVM chain: `chain` must match the network-level value.
          svm: { chain: "fogo", cluster: "mainnet", checkGenesisHash: true },
        },
      ],
      networks: [
        {
          architecture: "svm",
          svm: {
            cluster: "mainnet-beta",
            commitment: "confirmed",
            statePollerDebounce: "400ms",
            // Explicit 0 disables the finalized-slot lag filter outright.
            // Omitting the field takes the 100-slot default instead — the
            // two are different configurations, not synonyms.
            maxFinalizedSlotLag: 0,
            // Opt out of the getBlock slot-availability guard.
            enforceBlockAvailability: false,
          },
        },
        {
          architecture: "svm",
          svm: { chain: "fogo", cluster: "mainnet", commitment: "finalized" },
        },
      ],
    },
  ],
});

/* ───────────────── 2. Root-level constants are values, not just types ──── */

// Generated constants must be reachable from the entry point and usable in
// the position their name implies.
const _architecture: NetworkArchitecture = ArchitectureSvm as NetworkArchitecture;
const _upstreamType: UpstreamType = UpstreamTypeSvm as UpstreamType;

const _constantsDrivenConfig = createConfig({
  projects: [
    {
      id: "constants",
      upstreams: [{ type: _upstreamType, endpoint: "http://localhost:8899", svm: { cluster: "devnet" } }],
      networks: [{ architecture: _architecture, svm: { cluster: "devnet" } }],
    },
  ],
});

/* ───────────────────── 3. Standalone SVM types are usable ─────────────── */

// Consumers factor shared blocks out of their config; that requires the
// interfaces themselves, not just inline object literals.
const _sharedNetwork: SvmNetworkConfig = {
  cluster: "devnet",
  commitment: "processed",
  statePollerDebounce: 400,
  maxFinalizedSlotLag: 0,
  enforceBlockAvailability: false,
};

const _sharedUpstream: SvmUpstreamConfig = {
  chain: "solana",
  cluster: "devnet",
  checkGenesisHash: false,
};

const _sharedDefaults: SvmNetworkConfigForDefaults = {
  commitment: "finalized",
  enforceBlockAvailability: true,
};

/* ───────────────────── 4. Negative cases (must error) ─────────────────── */

createConfig({
  projects: [
    {
      id: "bad-architecture",
      // @ts-expect-error — "solana" is not an architecture; the value is "svm"
      networks: [{ architecture: "solana", svm: { cluster: "mainnet-beta" } }],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "bad-upstream-type",
      // @ts-expect-error — vendor-suffixed svm types are not supported yet
      upstreams: [{ type: "svm+helius", endpoint: "https://x", svm: { cluster: "mainnet-beta" } }],
    },
  ],
});

// The svm block is closed: EVM keys do not bleed into it, and neither do
// fields that were dropped from the Go schema (a stale key is a silent
// no-op in YAML, so the type layer is the only place it can be caught).
createConfig({
  projects: [
    {
      id: "closed-schema",
      networks: [
        {
          architecture: "svm",
          // @ts-expect-error — chainId belongs to the evm block, not svm
          svm: { cluster: "mainnet-beta", chainId: 1 },
        },
      ],
    },
  ],
});

createConfig({
  projects: [
    {
      id: "bad-lag-type",
      networks: [
        {
          architecture: "svm",
          // @ts-expect-error — maxFinalizedSlotLag is a number of slots, not a duration
          svm: { cluster: "mainnet-beta", maxFinalizedSlotLag: "100ms" },
        },
      ],
    },
  ],
});

const _clusterInDefaults: SvmNetworkConfigForDefaults = {
  // @ts-expect-error — cluster is per-network and omitted from the defaults type
  cluster: "mainnet-beta",
};

// Suppress "declared but never read" for the smoke values.
void _realisticSvmConfig;
void _constantsDrivenConfig;
void _sharedNetwork;
void _sharedUpstream;
void _sharedDefaults;
void _clusterInDefaults;
