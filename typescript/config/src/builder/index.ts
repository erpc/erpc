import type {
  Config,
  ProjectConfig,
  RateLimitBudgetConfig,
  RateLimitRuleConfig,
} from "../generated";
import type {
  AddProject,
  AddRateLimitersBudgets,
  AddToStore,
  BuilderMethodArgs,
  BuilderStore,
  BuilderStoreValues,
  ConfigAwareObject,
} from "../types/builder";

/**
 * Config Builder is here to ease the building of large erpc configuration.
 * Main advantages of using this builder compared to the simple default config export type are:
 *  - Strong type completion, on every fields of the configuration.
 *  - Reuse of shared configuration (upstreams, networks) between projects.
 *  - Extensability, you could  add your own module to the builder.
 *  - Better Dev experience, with the help of the IDE to build the configuration.
 */
class ConfigBuilder<
  TConfig extends Partial<Config> = {},
  TStore extends BuilderStore<string, string> = BuilderStore<never, never>,
> {
  private config: TConfig;
  private store: TStore;

  constructor(config: Omit<Config, "rateLimiters" | "projects">) {
    this.config = config as TConfig;
    this.store = {
      upstreams: {},
      networks: {},
    } as TStore;
  }

  /**
   * Adds rate limiters to the configuration.
   * All the rate limiters added using this method will automaticly be pushed to the final configuration.
   * This method provide strong type completion around every `rayteLimitBudget` field in the configuration (e.g. in auth, networks, upstreams etc).
   *
   * Example:
   * ```typescript
   * initErpcConfig({
   *   logLevel: "info",
   * })
   * .addRateLimiters({
   *   demoBudget: [
   *     {
   *       method: "eth_getBlockByNumber",
   *       waitTime: "1s",
   *       period: "1s",
   *       maxCount: 1000,
   *     },
   *   ],
   * })
   * .addProject({
   *   id: "test",
   *   rateLimitBudget: "demoBudget", // <- Strong type completion here, if you input a wrong budget id, you will get an error.
   * })
   * .build();
   * ```
   *
   * @param budgets A record where keys are budget identifiers and values are arrays of `RateLimitRuleConfig`.
   */
  addRateLimiters<
    TBudgets extends Record<
      string,
      [RateLimitRuleConfig, ...RateLimitRuleConfig[]]
    >,
  >(
    budgets: TBudgets,
  ): ConfigBuilder<
    AddRateLimitersBudgets<
      TConfig,
      keyof TBudgets extends string ? keyof TBudgets : never
    >,
    TStore
  > {
    // Format the rate limiters budgets
    const mappedRateLimiters: RateLimitBudgetConfig[] = Object.entries(
      budgets,
    ).map(
      ([id, rules]) =>
        ({
          id,
          rules,
        }) as unknown as RateLimitBudgetConfig,
    );

    // Push them to the configuration
    const rateLimiters = this.config.rateLimiters ?? { budgets: [] };
    rateLimiters.budgets.push(...mappedRateLimiters);
    this.config = {
      ...this.config,
      rateLimiters,
    };

    // Return this with the new types
    return this as unknown as ConfigBuilder<
      AddRateLimitersBudgets<
        TConfig,
        keyof TBudgets extends string ? keyof TBudgets : never
      >,
      TStore
    >;
  }

  /**
   * Adds a project to the configuration.
   * @param project The project configuration.
   */
  addProject<TProject extends ProjectConfig>(
    args: BuilderMethodArgs<ProjectConfig, TConfig, TStore>,
  ): ConfigBuilder<AddProject<TConfig, TProject>, TStore> {
    // Get the project from the args
    const project =
      typeof args === "function"
        ? args({ config: this.config, store: this.store })
        : args;

    // Rebuild the project array
    const newProjects = [...(this.config.projects || []), project];

    // Push them to the config
    this.config = {
      ...this.config,
      projects: newProjects,
    };

    // Return this with the new types
    return this as unknown as ConfigBuilder<
      AddProject<TConfig, TProject>,
      TStore
    >;
  }

  /**
   * Decorate the current builder with new values in it's store.
   * This help to add some shared networks / upstreams config inside the builder sotre. The valeus could then be retreived by the builder methods.
   * For multi projects erpc config, with a shared upstreams, it can looks like this:
   * ```typescript
   * initErpcConfig({
   *  logLevel: "info",
   * })
   * .decorate("upstreams", {
   *    mainNode: {
   *      endpoint: "https://my-main-node.com",
   *    }
   * })
   * .addProject(({ config, store : { upstreams } }) => ({
   *   id: "dev-rpc",
   *   upstreams: [upstreams.mainNode],
   * }))
   * .addProject(({ config, store : { upstreams } }) => ({
   *  id: "prod-rpc",
   *  upstreams: [upstreams.mainNode],
   * }))
   * .build();
   * ```
   *
   * @param scope The scope to decorate (`upstreams` or `networks`).
   * @param args The values to add to the store.
   * @returns The builder with the new values in the store.
   */
  decorate<
    TScope extends keyof BuilderStoreValues,
    TDecorationArgs extends Record<
      string,
      TScope extends keyof BuilderStoreValues
        ? BuilderStoreValues[TScope]
        : never
    >,
    TNewStoreKeys extends keyof TDecorationArgs,
  >(
    scope: TScope,
    value: ConfigAwareObject<TDecorationArgs, TConfig>,
  ): ConfigBuilder<
    TConfig,
    AddToStore<
      TStore,
      TScope,
      TNewStoreKeys extends string ? TNewStoreKeys : never
    >
  > {
    // Append that value to the store
    this.store[scope] = {
      ...this.store[scope],
      ...value,
    };

    return this as unknown as ConfigBuilder<
      TConfig,
      AddToStore<
        TStore,
        TScope,
        TNewStoreKeys extends string ? TNewStoreKeys : never
      >
    >;
  }

  /**
   * Builds the final configuration.
   * This should be the last statement of every builder chains, and if not used, the erpc server instance wouin't be able to resulve the current config.
   * @returns The final erpc `Config` object, containing everything we want.
   * @throws If the configuration isn't valid (e.g. no projects)
   */
  build(): Config {
    if (!this.config.projects || this.config.projects.length === 0) {
      throw new Error("No projects added to the configuration");
    }
    return this.config as Config;
  }
}

/**
 * Initializes the ERPC configuration builder.
 * Example:
 * ```typescript
 * initErpcConfig({
 *  logLevel: "info",
 * })
 *  // some configuration via builder here
 * .build();
 * ```
 *
 * @param `baseConfig` The erpc configuration without rate limiters and projects (log level, servers config, databases etc).
 * @returns A fresh builder instance.
 */
export function initErpcConfig<
  TInitial extends Omit<Config, "rateLimiters" | "projects">,
>(baseConfig: TInitial): ConfigBuilder<TInitial, BuilderStore<never, never>> {
  return new ConfigBuilder(baseConfig);
}
