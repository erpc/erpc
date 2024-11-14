# eRPC Typescript Config

This package contains the typescript definitions for the [eRPC config](https://github.com/erpc/erpc).

## Usage

To use this package, install it via npm:

```bash
npm install @erpc-cloud/config
```

And create a `erpc.ts` file:

```typescript
import { createConfig } from "@erpc-cloud/config";

export default createConfig({
    server: {
        httpHostV4: "0.0.0.0",
        httpPort: 4000,
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
```

And then run eRPC with the config:

```bash
docker run -v $(pwd)/erpc.ts:/root/erpc.ts -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```
