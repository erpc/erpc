import type { Prettify } from "viem";
import type { Config, ProjectConfig } from "../../generated";

/**
 * Add new rate limiters key to the configuration.
 */
export type AddProject<
  TConfig extends Partial<Config>,
  TProject extends ProjectConfig,
> = Prettify<
  TConfig extends {
    projects: { budgets: ProjectConfig[] };
  }
    ? TConfig & {
        projects: [...TConfig["projects"], TProject];
      }
    : TConfig & {
        projects: [TProject];
      }
>;
