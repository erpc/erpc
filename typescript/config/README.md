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
docker run -v $(pwd)/erpc.ts:/erpc.ts -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

## Installing NPM dependencies

If you have installed other dependencies besides @erpc-cloud/config, you must make sure the package.json and node_modules are also available in the container.

You can either build a new custom image with `erpc` as base, or mount them from your host machine.

### Building a custom image

Create a `Dockerfile.custom` file:
```dockerfile
FROM debian:12

COPY package.json pnpm-lock.yaml /
# COPY package.json package-lock.json /
# COPY package.json yarn.lock /

RUN pnpm install
# RUN npm install
# RUN yarn install

FROM ghcr.io/erpc/erpc:latest

COPY --from=0 /root/node_modules /node_modules
```

Then build the image:
```bash
docker build -t erpc-custom -f Dockerfile.custom .
```

### Mounting from host machine

In this case, you can use the `-v` flag to mount the package.json and node_modules from your host machine.

```bash
docker run \
    -v $(pwd)/package.json:/package.json \
    -v $(pwd)/node_modules:/node_modules \
    -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```
