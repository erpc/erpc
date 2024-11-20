export * from './types';
export type * from './types';
export { DataFinalityStateUnfinalized, DataFinalityStateFinalized, DataFinalityStateUnknown, } from './generated';
export type { Config, ServerConfig, AdminConfig, DatabaseConfig, ConnectorConfig, DataFinalityState, } from './generated';
import type { Config } from './generated';
export declare const createConfig: (cfg: Config) => Config;
//# sourceMappingURL=index.d.ts.map