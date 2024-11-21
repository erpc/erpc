import type { Config, ProjectConfig, RateLimitRuleConfig, ReplaceRateLimiter } from "../generated";
import type { BuilderMethodArgs, BuilderStore, BuilderStoreValues, DecorateArgs } from "../types/configBuilder";
/**
 * Object types observation:
 * - RateLimitRuleConfig.waitTime: Could be optional?
 */
/**
 * Top level erpc config builder
 */
declare class ConfigBuilder<TConfig extends Partial<Config> = {}, TRateLimitBudgetKeys extends string = never, TStore extends BuilderStore<string, string> = BuilderStore<never, never>> {
    private config;
    private store;
    constructor(config: TConfig);
    /**
     * Adds rate limiters to the configuration.
     * @param budgets A record where keys are budget identifiers and values are arrays of RateLimitRuleConfig.
     */
    addRateLimiters<TBudgets extends Record<string, RateLimitRuleConfig[]>>(args: BuilderMethodArgs<TBudgets, TConfig, TStore>): ConfigBuilder<TConfig & {
        rateLimiters: {
            budgets: TBudgets;
        };
    }, TRateLimitBudgetKeys | (keyof TBudgets & string)>;
    /**
     * Adds a project to the configuration.
     * @param project The project configuration.
     */
    addProject<TProject extends ProjectConfig>(args: BuilderMethodArgs<ReplaceRateLimiter<ProjectConfig, TRateLimitBudgetKeys>, TConfig, TStore>): ConfigBuilder<TConfig & {
        projects: [
            ...(TConfig["projects"] extends any[] ? TConfig["projects"] : []),
            TProject
        ];
    }, TRateLimitBudgetKeys>;
    /**
     * Decorate the current builder with new values in it's store.
     */
    decorate<TScope extends keyof TStore, TNewKeys extends string>(args: DecorateArgs<TScope, TNewKeys, TRateLimitBudgetKeys, TStore, TConfig>): ConfigBuilder<TConfig, TRateLimitBudgetKeys, TStore & {
        [scope in TScope]: Record<keyof TStore[TScope] extends never ? TNewKeys : keyof TStore[TScope] | TNewKeys, TScope extends keyof BuilderStoreValues ? BuilderStoreValues[TScope] : never>;
    }>;
    /**
     * Builds the final configuration.
     */
    build(): TConfig extends Config ? Config : never;
}
/**
 * Initializes the ERPC configuration builder.
 * @param baseConfig The base configuration without rate limiters and projects.
 * @returns A new builder instance.
 */
export declare function initErpcConfig<T extends Omit<Config, "rateLimiters" | "projects">>(baseConfig: T): ConfigBuilder<T, never>;
export {};
//# sourceMappingURL=index.d.ts.map