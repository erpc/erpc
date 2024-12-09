export * from './types'
export type * from './types'
export {
  DataFinalityStateUnfinalized,
  DataFinalityStateFinalized,
  DataFinalityStateUnknown,
  DataFinalityStateRealtime,
  CacheEmptyBehaviorAllow,
  CacheEmptyBehaviorIgnore,
  CacheEmptyBehaviorOnly,
} from './generated'
export type {
  CacheConfig,
  Config,
} from './generated'

import type { Config } from './generated'

export const createConfig = (
  cfg: Config
): Config => {
  return cfg;
};