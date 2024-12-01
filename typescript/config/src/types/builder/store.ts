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
> = {
  // Iterate over each scope in the current store
  [PrevScope in keyof TStore]: PrevScope extends TScope
    ? {
        // If the scope is the target scope, add the new keys to the store
        [StoreKey in keyof TStore[PrevScope] | TNewKeys]: StoreKey extends never
          ? never
          : BuilderStoreValues[PrevScope];
      }
    : TStore[PrevScope]; // If not target scope, just put previous store values
};
