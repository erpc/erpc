/**
 * Example config in TypeScript, copy and create erpc.ts so the binary automatically imports it.
 */
import { createConfig, DataFinalityStateFinalized, DataFinalityStateRealtime, DataFinalityStateUnfinalized } from "@erpc-cloud/config";

export default createConfig({
    logLevel: "trace",
    server: {
        listenV4: true,
        httpHostV4: "0.0.0.0",
        httpPort: 4000,
    },
    database: {
        evmJsonRpcCache: {
            connectors: [
                {
                    id: "default-memory",
                    driver: "memory",
                    memory: {
                        maxItems: 100000,
                        maxTotalSize: "1GB"
                    }
                }
            ],
            policies: [
                {
                    connector: 'default-memory',
                    network: '*',
                    method: '*',
                    finality: DataFinalityStateUnfinalized,
                    ttl: '10s'
                },
                {
                    connector: 'default-memory',
                    network: '*',
                    method: '*',
                    finality: DataFinalityStateFinalized,
                    ttl: '0s'
                }
            ]
        }
    },
    projects: [
        {
            id: "main",
            networks: [
                {
                    architecture: "evm",
                    evm: {
                        chainId: 1
                    },
                    failsafe: [
                        {
                            matchMethod: "*",
                            matchFinality: [DataFinalityStateRealtime],
                            timeout: {
                                duration: "3s"
                            },
                            retry: {
                                maxAttempts: 2
                            }
                        },
                        {
                            matchMethod: "eth_getLogs",
                            timeout: {
                                duration: "5s"
                            },
                            retry: {
                                maxAttempts: 5,

                            }
                        },
                        {
                            matchMethod: "*",
                            timeout: {
                                duration: "10s"
                            },
                            retry: {
                                maxAttempts: 3
                            }
                        }
                    ]
                }
            ],
            upstreams: [
                {
                    endpoint: `alchemy://${process.env.ALCHEMY_API_KEY}`
                },
                {
                    endpoint: `chainstack://${process.env.CHAINSTACK_API_KEY}`
                },
                {
                    endpoint: `${process.env.CUSTOM_RPC_1}`
                }
            ]
        }
    ]
})