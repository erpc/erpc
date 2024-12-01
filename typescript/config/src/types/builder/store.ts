import type { Prettify } from "viem";
import type { NetworkConfig, UpstreamConfig } from "../../generated";

/**
 * The store for the builder, containg some helper methods
 */
export type BuilderStore<
  TUpstreamKeys extends keyof any,
  TNetworkKeys extends keyof any,
> = {
  upstreams: Record<TUpstreamKeys, BuilderStoreValues["upstreams"]>;
  networks: Record<TNetworkKeys, BuilderStoreValues["networks"]>;
};

/**
 * The raw values stored in the builder store
 */
export type BuilderStoreValues = {
  upstreams: UpstreamConfig;
  networks: NetworkConfig;
};

/**
 * Simple representation of the builder store, to ease type access
 */
export type AnyBuilderStore = BuilderStore<string, string>;

/**
 * Helper types to add new `TNewKeys` keys to the store in the `TScope` scope.
 */
export type AddToStore<
  TStore extends AnyBuilderStore,
  TScope extends keyof BuilderStoreValues,
  TNewKeys extends string,
  TValue extends BuilderStoreValues[TScope] extends never
    ? never
    : BuilderStoreValues[TScope] = BuilderStoreValues[TScope],
> = {
  // Iterate over each scope in the current store
  [PrevScope in keyof TStore]: PrevScope extends TScope
    ? Prettify<TStore[PrevScope] & Record<TNewKeys, TValue>> // Add the new keys to the store
    : TStore[PrevScope]; // If not target scope, just put previous store values
};
