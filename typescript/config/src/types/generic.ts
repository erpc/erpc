import type {
  DynamoDBConnectorConfig,
  MemoryConnectorConfig,
  PostgreSQLConnectorConfig,
  RedisConnectorConfig,
} from "../generated";

/**
 * Possible log level configuration
 */
export type LogLevel =
  | "trace"
  | "debug"
  | "info"
  | "warn"
  | "error"
  | "disabled"
  | undefined;

/**
 * Generic type representing a time.Duration config
 */
export type Duration =
  | `${number}ms`
  | `${number}s`
  | `${number}m`
  | `${number}h`;

/**
 * Suported network architecture
 */
export type NetworkArchitecture = "evm";

/**
 * Supported connector driver type overide
 */
export type ConnectorDriverType = "memory" | "redis" | "postgres" | "dynamodb";

/**
 * Connector config depending on the upstream type
 */
export type ConnectorConfig =
  | {
      id: string;
      driver: "memory";
      memory: MemoryConnectorConfig;
    }
  | {
      id: string;
      driver: "redis";
      redis: RedisConnectorConfig;
    }
  | {
      id: string;
      driver: "dynamodb";
      dynamodb: DynamoDBConnectorConfig;
    }
  | {
      id: string;
      driver: "postgresql";
      postgresql: PostgreSQLConnectorConfig;
    };

/**
 * Supported upstream type
 */
export type UpstreamType =
  | "evm"
  | "evm+alchemy"
  | "evm+drpc"
  | "evm+blastapi"
  | "evm+envio"
  | "evm+etherspot"
  | "evm+infura"
  | "evm+pimlico"
  | "evm+thirdweb";
