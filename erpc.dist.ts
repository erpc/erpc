/**
 * Example config in TypeScript, copy and create erpc.ts so the binary automatically imports it.
 */
import { createConfig } from "@erpc-cloud/config";

export default createConfig({
    logLevel: "trace",
    server: {
        httpHostV4: "0.0.0.0",
        httpPort: 4000,
    },
    admin: {
        auth: {
            strategies: [
                {
                    type: "secret",
                    secret: {
                        value: "admin"
                    }
                }
            ]
        }
    },
    projects: [
        {
            id: "main",
            networks: [
                {
                    architecture: 'evm',
                    evm: {
                        chainId: 123
                    },
                    selectionPolicy: {
                        evalInterval: '5s',
                        evalFunction: function (upstreams, method) {
                            const defaults = upstreams.filter(u => u.config.group !== 'fallback')
                            const fallbacks = upstreams.filter(u => u.config.group === 'fallback')
                            const maxErrorRate = parseFloat(process.env.ROUTING_POLICY_ERROR_RATE || '0.7')
                            const maxBlockHeadLag = parseFloat(process.env.ROUTING_POLICY_BLOCK_LAG || '10')
                            const healthyOnes = defaults.filter(
                                u => u.metrics.errorRate < maxErrorRate || u.metrics.blockHeadLag < maxBlockHeadLag
                            )
                            if (healthyOnes.length > 0) {
                                return healthyOnes
                            }
                            return [...healthyOnes, ...fallbacks]
                        }
                    }
                }
            ],
            upstreams: [
                {
                    endpoint: "http://localhost:9081",
                    evm: {
                        chainId: 123
                    }
                },
                {
                    endpoint: "http://localhost:9082",
                    evm: {
                        chainId: 123
                    }
                }
            ]
        }
    ]
})