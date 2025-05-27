import type { DynamoDBConnectorConfig, AuthStrategyConfig as GenAuthStrategyConfig, JwtStrategyConfig, MemoryConnectorConfig, NetworkStrategyConfig, PostgreSQLConnectorConfig, RedisConnectorConfig, SecretStrategyConfig, SiweStrategyConfig } from "../generated";
/**
 * Possible log level configuration
 */
export type LogLevel = "trace" | "debug" | "info" | "warn" | "error" | "disabled" | undefined;
/**
 * Generic type representing a time duration with units (ms, s, m, h),
 * or a number that will be interpreted as milliseconds.
 */
export type Duration = `${number}ms` | `${number}s` | `${number}m` | `${number}h` | number;
/**
 * Generic type representing size with units (b, kb, mb), or a number that will be interpreted as bytes.
 */
export type ByteSize = `${number}kb` | `${number}mb` | `${number}b` | number;
/**
 * Suported network architecture
 */
export type NetworkArchitecture = "evm";
/**
 * Supported connector driver type overide
 */
export type ConnectorDriverType = "memory" | "redis" | "postgresql" | "dynamodb";
/**
 * Connector config depending on the upstream type
 */
export type ConnectorConfig = {
    id: string;
    driver: "memory";
    memory: MemoryConnectorConfig;
} | {
    id: string;
    driver: "redis";
    redis: RedisConnectorConfig;
} | {
    id: string;
    driver: "dynamodb";
    dynamodb: DynamoDBConnectorConfig;
} | {
    id: string;
    driver: "postgresql";
    postgresql: PostgreSQLConnectorConfig;
};
/**
 * Supported upstream type
 */
export type UpstreamType = "evm" | "evm+alchemy" | "evm+blastapi" | "evm+conduit" | "evm+drpc" | "evm+dwellir" | "evm+envio" | "evm+etherspot" | "evm+infura" | "evm+pimlico" | "evm+quicknode" | "evm+llama" | "evm+thirdweb" | "evm+repository" | "evm+superchain" | "evm+chainstack" | "evm+tenderly" | "evm+onfinality";
/**
 * Supported auth type
 */
export type AuthType = "secret" | "jwt" | "siwe" | "network";
/**
 * Connector config depending on the upstream type
 */
export type AuthStrategyConfig = Omit<GenAuthStrategyConfig, "type" | "network" | "secret" | "jwt" | "siwe"> & ({
    type: "secret";
    secret: SecretStrategyConfig;
} | {
    type: "network";
    secret: NetworkStrategyConfig;
} | {
    type: "jwt";
    secret: JwtStrategyConfig;
} | {
    type: "siwe";
    secret: SiweStrategyConfig;
});
//# sourceMappingURL=generic.d.ts.map