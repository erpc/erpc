export * from "./types";
export type * from "./types";
export { DataFinalityStateUnfinalized, DataFinalityStateFinalized, DataFinalityStateUnknown, } from "./generated";
export type { Config, ServerConfig, AdminConfig, DatabaseConfig, ConnectorConfig, DataFinalityState, } from "./generated";
export { initErpcConfig } from "./builder";
import type { Config } from "./generated";
/**
 * Create a new config object.
 * @param cfg The config object.
 */
export declare const createConfig: (cfg: Config) => Config;
//# sourceMappingURL=index.d.ts.map