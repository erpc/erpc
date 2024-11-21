import type {
  Config,
  ProjectConfig,
  RateLimitBudgetConfig,
  RateLimitRuleConfig,
  ReplaceRateLimiter,
} from "../generated";
import type {
  BuilderMethodArgs,
  BuilderStore,
  BuilderStoreValues,
  DecorateArgs,
} from "../types/configBuilder";

/**
 * Object types observation:
 * - RateLimitRuleConfig.waitTime: Could be optional?
 */

/**
 * Top level erpc config builder
 */
class ConfigBuilder<
  TConfig extends Partial<Config> = {},
  TRateLimitBudgetKeys extends string = never,
  TStore extends BuilderStore<string, string> = BuilderStore<never, never>,
> {
  private config: TConfig;
  private store: TStore;

  constructor(config: TConfig) {
    this.config = config as TConfig;
    this.store = {
      upstreams: {},
      networks: {},
    } as TStore;
  }

  /**
   * Adds rate limiters to the configuration.
   * @param budgets A record where keys are budget identifiers and values are arrays of RateLimitRuleConfig.
   */
  addRateLimiters<TBudgets extends Record<string, RateLimitRuleConfig[]>>(
    args: BuilderMethodArgs<TBudgets, TConfig, TStore>,
  ): ConfigBuilder<
    TConfig & { rateLimiters: { budgets: TBudgets } },
    TRateLimitBudgetKeys | (keyof TBudgets & string)
  > {
    // Get the budgets from the args
    const budgets =
      typeof args === "function"
        ? args({ config: this.config, store: this.store })
        : args;

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
  addProject<TProject extends ProjectConfig>(
    args: BuilderMethodArgs<
      ReplaceRateLimiter<ProjectConfig, TRateLimitBudgetKeys>,
      TConfig,
      TStore
    >,
  ): ConfigBuilder<
    TConfig & {
      projects: [
        ...(TConfig["projects"] extends any[] ? TConfig["projects"] : []),
        TProject,
      ];
    },
    TRateLimitBudgetKeys
  > {
    // Get the project from the args
    const project =
      typeof args === "function"
        ? args({ config: this.config, store: this.store })
        : args;

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
   * Decorate the current builder with new values in it's store.
   */
  decorate<TScope extends keyof TStore, TNewKeys extends string>(
    args: DecorateArgs<TScope, TNewKeys, TRateLimitBudgetKeys, TStore, TConfig>,
  ): ConfigBuilder<
    TConfig,
    TRateLimitBudgetKeys,
    TStore & {
      [scope in TScope]: Record<
        keyof TStore[TScope] extends never
          ? TNewKeys
          : keyof TStore[TScope] | TNewKeys,
        TScope extends keyof BuilderStoreValues
          ? BuilderStoreValues[TScope]
          : never
      >;
    }
  > {
    // Extract the value we want to add
    const { scope, value } =
      typeof args === "function"
        ? args({ config: this.config, store: this.store })
        : args;

    // Append that value to the store
    this.store[scope] = {
      ...this.store[scope],
      ...value,
    };

    return this as unknown as ConfigBuilder<
      TConfig,
      TRateLimitBudgetKeys,
      TStore & {
        [scope in TScope]: Record<
          keyof TStore[TScope] extends never
            ? TNewKeys
            : keyof TStore[TScope] | TNewKeys,
          TScope extends keyof BuilderStoreValues
            ? BuilderStoreValues[TScope]
            : never
        >;
      }
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
