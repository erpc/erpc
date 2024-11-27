import type { Config, NetworkConfig, UpstreamConfig } from "../generated";

/**
 * Replace every "rateLimitBudget: string" in the object and every sub ojectif, with a "rateLimitBudget: RateLimiterIds[number]"
 */
export type ReplaceRateLimiter<
  T extends Object,
  RateLimiterIds extends string,
> = (T extends {
  rateLimitBudget?: string;
}
  ? Omit<T, "rateLimitBudget"> & { rateLimitBudget?: RateLimiterIds }
  : T) & {
  [K in keyof T]: T[K] extends Object
    ? ReplaceRateLimiter<T[K], RateLimiterIds>
    : T[K];
};

/**
 * Simple method to get the type of a function.
 */
export type ObjectOrFunction<TOutput, TArgs> =
  | TOutput
  | (TArgs extends undefined ? () => TOutput : (args: TArgs) => TOutput);

/**
 * Arguments for a builder method.
 */
export type BuilderMethodArgs<
  TOutput,
  TConfig extends Partial<Config>,
  TStore extends BuilderStore<string, string>,
> = ObjectOrFunction<TOutput, { config: TConfig; store: TStore }>;

/**
 * Store for the builder.
 */
export type BuilderStore<
  TUpstreamKeys extends string = never,
  TNetworkKeys extends string = never,
> = {
  upstreams: Record<TUpstreamKeys, BuilderStoreValues["upstreams"]>;
  networks: Record<TNetworkKeys, BuilderStoreValues["networks"]>;
};
type AnyBuilderStore = BuilderStore<string, string>;
export type BuilderStoreValues = {
  upstreams: UpstreamConfig;
  networks: NetworkConfig;
};

/**
 * The decorate method will be used to push stuff to our store, so it should be:
 */
export type DecorateArgs<
  TScope extends keyof TStore,
  TNewKeys extends string,
  TRateLimitBudgetKeys extends string,
  TStore extends AnyBuilderStore,
  TConfig extends Partial<Config>,
> = BuilderMethodArgs<
  {
    scope: TScope;
    value: ReplaceRateLimiter<
      Record<
        TNewKeys,
        TScope extends keyof BuilderStoreValues
          ? BuilderStoreValues[TScope]
          : never
      >,
      TRateLimitBudgetKeys
    >;
  },
  TConfig,
  TStore
>;
