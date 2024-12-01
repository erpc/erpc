import type { Prettify } from "viem";
import type { Config, RateLimitBudgetConfig } from "../../generated";

/**
 * Add new rate limiters key to the configuration.
 */
export type AddRateLimitersBudgets<
  TConfig extends Partial<Config>,
  TNewKeys extends string,
> = Prettify<
  TConfig extends {
    rateLimiters: { budgets: RateLimitBudgetConfig[] };
  }
    ? TConfig & {
        rateLimiters: {
          budgets: Prettify<
            Omit<RateLimitBudgetConfig, "id"> & {
              id: TNewKeys | RateLimiterIdsFromConfig<TConfig>;
            }
          >[];
        };
      }
    : TConfig & {
        rateLimiters: {
          budgets: Prettify<
            Omit<RateLimitBudgetConfig, "id"> & { id: TNewKeys }
          >[];
        };
      }
>;

/**
 * Helper types to retreive the supported rate limiter ids from a Config object
 */
export type RateLimiterIdsFromConfig<TConfig extends Partial<Config>> =
  TConfig extends {
    rateLimiters: { budgets: { id: string }[] };
  }
    ? TConfig["rateLimiters"]["budgets"][number]["id"]
    : never;

/**
 * Replace every "rateLimitBudget: string" in the object and every sub ojectif, with a "rateLimitBudget: RateLimiterIds[number]"
 */
export type ReplaceRateLimiter<
  T extends Object,
  RateLimiterIds extends string,
> = Prettify<
  (T extends {
    rateLimitBudget?: string;
  }
    ? Omit<T, "rateLimitBudget"> & { rateLimitBudget?: RateLimiterIds }
    : T) & {
    [K in keyof T]: T[K] extends Object
      ? ReplaceRateLimiter<T[K], RateLimiterIds>
      : T[K];
  }
>;
