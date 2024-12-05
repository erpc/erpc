import type { Config, ProjectConfig } from "../../generated";

/**
 * Add new rate limiters key to the configuration.
 */
export type AddProject<
  TConfig extends Partial<Config>,
  TProject extends ProjectConfig,
> = TConfig extends {
  projects: any[];
}
  ? {
      // Config already included a projects key, add the new project to the array
      [K in keyof TConfig]: K extends "projects"
        ? [TProject, ...TConfig[K]]
        : TConfig[K];
    }
  : {
      // Config didn't include a projects before, add the key with the new project
      [K in keyof TConfig | "projects"]: K extends "projects"
        ? [TProject]
        : TConfig[K];
    };
