import type {
  Config,
  ProjectConfig,
  RateLimitBudgetConfig,
  RateLimitRuleConfig,
  ReplaceRateLimiter,
} from "../generated";

/**
 * Top level erpc config builder
 */
class ConfigBuilder<
  TConfig extends Partial<Config> = {},
  TRateLimitBudgetKeys extends string = never,
> {
  private config: TConfig;

  constructor(config: TConfig) {
    this.config = config as TConfig;
  }

  /**
   * Adds rate limiters to the configuration.
   * @param budgets A record where keys are budget identifiers and values are arrays of RateLimitRuleConfig.
   */
  addRateLimiters<TBudgets extends Record<string, RateLimitRuleConfig[]>>(
    budgets: TBudgets,
  ): ConfigBuilder<
    TConfig & { rateLimiters: { budgets: TBudgets } },
    TRateLimitBudgetKeys | (keyof TBudgets & string)
  > {
    // Format the rate limiters budgets
    const mappedRateLimiters: RateLimitBudgetConfig[] = Object.entries(
      budgets,
    ).map(([id, rules]) => ({
      id,
      rules,
    }));

    // Push them to the configuration
    const rateLimiters = this.config.rateLimiters ?? { budgets: [] };
    rateLimiters.budgets.push(...mappedRateLimiters);
    this.config = {
      ...this.config,
      rateLimiters,
    } as TConfig & { rateLimiters: { budgets: TBudgets } };

    // Return this with the new types
    return this as unknown as ConfigBuilder<
      TConfig & { rateLimiters: { budgets: TBudgets } },
      TRateLimitBudgetKeys | (keyof TBudgets & string)
    >;
  }

  /**
   * Adds a project to the configuration.
   * @param project The project configuration.
   */
  addProject<
    TProject extends ProjectConfig & { rateLimitBudget?: TRateLimitBudgetKeys },
  >(
    project: ReplaceRateLimiter<ProjectConfig, TRateLimitBudgetKeys>,
  ): ConfigBuilder<
    TConfig & {
      projects: [
        ...(TConfig["projects"] extends any[] ? TConfig["projects"] : []),
        TProject,
      ];
    },
    TRateLimitBudgetKeys
  > {
    // Rebuild the project array
    const newProjects = [
      ...(this.config.projects || []),
      project,
    ] as TProject[];

    // Push them to the config
    this.config = {
      ...this.config,
      projects: newProjects,
    } as TConfig;

    // Return this with the new types
    return this as unknown as ConfigBuilder<
      TConfig & {
        projects: [
          ...(TConfig["projects"] extends any[] ? TConfig["projects"] : []),
          TProject,
        ];
      },
      TRateLimitBudgetKeys
    >;
  }

  /**
   * Builds the final configuration.
   */
  build(): TConfig extends Config ? Config : never {
    return this.config as any;
  }
}

/**
 * Initializes the ERPC configuration builder.
 * @param baseConfig The base configuration without rate limiters and projects.
 * @returns A new builder instance.
 */
export function initErpcConfig<
  T extends Omit<Config, "rateLimiters" | "projects">,
>(baseConfig: T): ConfigBuilder<T, never> {
  return new ConfigBuilder<T, never>(baseConfig);
}
