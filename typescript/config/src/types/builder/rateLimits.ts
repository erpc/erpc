import type { Config, RateLimitBudgetConfig } from "../../generated";

/**
 * Add new rate limiters key to the configuration.
 */
export type AddRateLimitersBudgets<
  TConfig extends Partial<Config>,
  TNewKeys extends string,
> = TConfig extends {
  rateLimiters: { budgets: RateLimitBudgetConfig[] };
}
  ? TConfig & {
      rateLimiters: {
        budgets: (Omit<RateLimitBudgetConfig, "id"> & {
          id: TNewKeys | TConfig["rateLimiters"]["budgets"][number]["id"];
        })[];
      };
    }
  : TConfig & {
      rateLimiters: {
        budgets: (Omit<RateLimitBudgetConfig, "id"> & { id: TNewKeys })[];
      };
    };

/**
 * Helper types to retreive the supported rate limiter ids from a Config object
 */
export type RateLimiterIdsFromConfig<TConfig extends Partial<Config>> =
  TConfig extends {
    rateLimiters: { budgets: { id: string }[] };
  }
    ? TConfig["rateLimiters"]["budgets"][number]["id"]
    : string & {};

/**
 * Replace every "rateLimitBudget: string" in the object and every sub ojectif, with a "rateLimitBudget: RateLimiterIds[number]"
 */
export type ReplaceRateLimiter<
  T extends Object,
  RateLimiterIds extends string,
> = {
  // Iterate over all the key of our object
  [ObjKey in keyof T]: ObjKey extends "rateLimitBudget"
    ? RateLimiterIds // If the key is "rateLimitBudget", replace the value with RateLimiterIds
    : T[ObjKey] extends Object // Otherwise, check if the key is a subobject, if that's the case, call ReplaceRateLimiter recursively
      ? ReplaceRateLimiter<T[ObjKey], RateLimiterIds>
      : T[ObjKey];
};
