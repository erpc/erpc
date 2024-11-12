export type * from './types'
export type {
    Config,
    ServerConfig,
    AdminConfig,
    DatabaseConfig,
    ConnectorConfig,
} from './generated'

import type { Config } from './generated'

export const createConfig = (cfg: Config) => {
    return cfg
}
