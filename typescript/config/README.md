# eRPC Typescript Config

This package contains the typescript definitions for the [eRPC config](https://github.com/erpc/erpc).

## Usage

To use this package, install it via npm:

```bash
npm install @erpc-cloud/config
```

And create a `erpc.ts` file:

### Direct configuration export

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

### Builder configuration export

## Builder usage

You could also use the builder to generate the config:

```typescript
import { initErpcConfig } from "@erpc-cloud/config";

export default initErpcConfig({
  logLevel: "info",
  server: {
    httpHostV4: "0.0.0.0",
    httpPort: 4000,
  },
})
  .addRateLimiters({
    budget1: [
      {
        method: "*",
        period: "1s",
        maxCount: 10,
        waitTime: "1s",
      },
    ]
  })
  .addProject({
    id: "project1",
    rateLimitBudget: "budget1",
    upstreams: [
      {
        id: "test",
        endpoint: "http://localhost:3000",
        rateLimitBudget: "budget1",
      },
    ],
  })
  .build();
```

### Running eRPC

And then run eRPC with the config:

```bash
docker run -v $(pwd)/erpc.ts:/root/erpc.ts -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

## Installing NPM dependencies

If you have installed other dependencies besides @erpc-cloud/config, you must make sure the package.json and node_modules are also available in the container.

You can either build a new custom image with `erpc` as base, or mount them from your host machine.

### Building a custom image

Create a `Dockerfile.custom` file:
```dockerfile
FROM debian:12

COPY package.json pnpm-lock.yaml /root/
# COPY package.json package-lock.json /root/
# COPY package.json yarn.lock /root/

RUN pnpm install
# RUN npm install
# RUN yarn install

FROM ghcr.io/erpc/erpc:latest

COPY --from=0 /root/node_modules /root/node_modules
```

Then build the image:
```bash
docker build -t erpc-custom -f Dockerfile.custom .
```

### Mounting from host machine

In this case, you can use the `-v` flag to mount the package.json and node_modules from your host machine.

```bash
docker run \
    -v $(pwd)/package.json:/root/package.json \
    -v $(pwd)/node_modules:/root/node_modules \
    -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

