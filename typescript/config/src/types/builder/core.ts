import type { Config } from "../../generated";
import type {
  RateLimiterIdsFromConfig,
  ReplaceRateLimiter,
} from "./rateLimits";
import type { AnyBuilderStore } from "./store";
import type { ObjectOrFunction } from "./utils";

/**
 * Generic builder context that will be sent to every builder method.
 */
export type BuilderContext<
  TConfig extends Config,
  TStore extends AnyBuilderStore,
> = {
  config: TConfig;
  store: TStore;
};

/**
 * Arguments for a builder method.
 */
export type BuilderMethodArgs<
  TOutput extends Object,
  TConfig extends Partial<Config>,
  TStore extends AnyBuilderStore,
> = ObjectOrFunction<
  ConfigAwareObject<TOutput, TConfig>,
  { config: TConfig; store: TStore }
>;

/**
 * Object that is config aware
 *  - For now, it's only replacing the rate limiters
 *
 * todo:
 *  - Also replace `connector` from `CachePolicyConfig` with known `connectors.id`
 */
export type ConfigAwareObject<
  TObject extends Object,
  TConfig extends Partial<Config>,
> = ReplaceRateLimiter<TObject, RateLimiterIdsFromConfig<TConfig>>;
