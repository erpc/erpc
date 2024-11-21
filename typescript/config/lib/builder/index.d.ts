import type { Config, ProjectConfig, RateLimitRuleConfig, ReplaceRateLimiter } from "../generated";
/**
 * Top level erpc config builder
 */
declare class ConfigBuilder<TConfig extends Partial<Config> = {}, TRateLimitBudgetKeys extends string = never> {
    private config;
    constructor(config: TConfig);
    /**
     * Adds rate limiters to the configuration.
     * @param budgets A record where keys are budget identifiers and values are arrays of RateLimitRuleConfig.
     */
    addRateLimiters<TBudgets extends Record<string, RateLimitRuleConfig[]>>(budgets: TBudgets): ConfigBuilder<TConfig & {
        rateLimiters: {
            budgets: TBudgets;
        };
    }, TRateLimitBudgetKeys | (keyof TBudgets & string)>;
    /**
     * Adds a project to the configuration.
     * @param project The project configuration.
     */
    addProject<TProject extends ProjectConfig & {
        rateLimitBudget?: TRateLimitBudgetKeys;
    }>(project: ReplaceRateLimiter<ProjectConfig, TRateLimitBudgetKeys>): ConfigBuilder<TConfig & {
        projects: [
            ...(TConfig["projects"] extends any[] ? TConfig["projects"] : []),
            TProject
        ];
    }, TRateLimitBudgetKeys>;
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