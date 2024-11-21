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
