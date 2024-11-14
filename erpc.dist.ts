/**
 * Example config in TypeScript, copy and create erpc.ts so the binary automatically imports it.
 */
import { createConfig } from "@erpc-cloud/config";

export default createConfig({
    logLevel: "info",
    server: {
        listenV4: true,
        httpHostV4: "0.0.0.0",
        httpPort: 4000,
    },
    projects: [
        {
            id: "main",
            upstreams: [
                {
                    endpoint: "alchemy://xxxxxxxxxxxxxxxxx"
                },
                {
                    endpoint: "http://localhost:9082"
                }
            ]
        }
    ]
})