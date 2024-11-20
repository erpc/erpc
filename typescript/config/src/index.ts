export * from './types'
export type * from './types'
export {
    DataFinalityStateUnfinalized,
    DataFinalityStateFinalized,
    DataFinalityStateUnknown,
} from './generated'
export type {
    Config,
    ServerConfig,
    AdminConfig,
    DatabaseConfig,
    ConnectorConfig,
    DataFinalityState,
} from './generated'

import type { Config } from './generated'

export const createConfig = (
  cfg: Config
): Config => {
  return cfg;
};