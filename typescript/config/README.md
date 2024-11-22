# eRPC Typescript Config

This package contains the TypeScript definitions for the [eRPC config](https://github.com/erpc/erpc).

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

The builder provides a flexible way to create your eRPC configuration, with several helper methods available. All methods are strongly typed to ensure type safety and prevent runtime errors.

#### Example Usage

```typescript
import { initErpcConfig } from "@erpc-cloud/config";

export default initErpcConfig({
  logLevel: "debug",
})
  .addRateLimiters({
    rateLimiter1: [
      {
        method: "*",
        maxCount: 1000,
        period: "1m",
        waitTime: "1s",
      },
    ],
  })
  .decorate({
    scope: "upstreams",
    value: {
      upstream1: {
        endpoint: "http://localhost:3000",
      },
    },
  })
  .addProject(({ store: { upstreams } }) => ({
    id: "project1",
    upstreams: [upstreams.upstream1],
  }));
```

#### Available Methods

Every builder method accepts either the arguments directly or a function that takes the current configuration and store, and returns the expected arguments.

- **`initErpcConfig(config: InitConfig)`**: Initializes the configuration with basic settings such as `logLevel` and `server` properties.

- **`addRateLimiters(options: Record\<TKeys,RateLimitRuleConfig[]>)`**: Adds rate limiters to your configuration.

- **`decorate(options: { scope: "upstreams" | "networks"; value: NetworkConfig | UpstreamConfig)`**: Adds values to the store for `networks` or `upstreams`.

  - **Scope**: Determines which store (`networks` or `upstreams`) to push values to.
  - **Value**: Either a static configuration or a function that returns the expected configuration.

- **`addProject(project: ProjectConfig)`**: Adds a project to your configuration.

#### Example Usage

```typescript
import { initErpcConfig } from "@erpc-cloud/config";

export default initErpcConfig({
  logLevel: "debug",
})
  .addRateLimiters({
    rpcLimiter: [
      {
        method: "eth_getLogs",
        maxCount: 100,
        period: "1m",
        waitTime: "1s",
      },
    ],
    projectLimiter: [
      {
        method: "*",
        maxCount: 1000,
        period: "1m",
        waitTime: "1s",
      },
    ],
  })
  .decorate({
    scope: "upstreams",
    value: {
      upstream1: {
        endpoint: "http://localhost:3000",
        rateLimitBudget: "rpcLimiter",
      },
    },
  })
  .addProject(({ store: { upstreams } }) => ({
    id: "project1",
    upstreams: [upstreams.upstream1],
    rateLimitBudget: "projectLimiter",
  }));
```

### Running eRPC

And then run eRPC with the config:

```bash
docker run -v $(pwd)/erpc.ts:/root/erpc.ts -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

## Optimized Docker Usage with TypeScript

If your TypeScript config file has external dependencies, it's best to bundle them into a single JavaScript file to reduce image size and improve performance.

### Dockerfile Example

Create a `Dockerfile.custom` file:

```dockerfile
# Config bundler step
FROM oven/bun:latest AS bundler
RUN mkdir -p /temp/dev

# Bundle everything in a single erpc.js file
COPY . /temp/dev
RUN cd /temp/dev && bun install
RUN cd /temp/dev && bun build --outfile ./erpc.js --minify --target node erpc.ts

# Final image
FROM ghcr.io/erpc/erpc:latest AS final

# Copy the bundled config
COPY --from=bundler ./temp/dev/erpc.js /root/erpc.js

# Run the server
CMD ["./erpc-server"]
```

This example uses Bun to bundle the `erpc.ts` configuration and dependencies into a single `erpc.js` file. You can use any bundler you're comfortable with (e.g., `tsup`, `terser`).

### Alternative Method

You can also copy the `node_modules` directory and the TypeScript config file into the container, but this will result in a heavier image as all dependencies are included.

```bash
docker run \
    -v $(pwd)/package.json:/root/package.json \
    -v $(pwd)/node_modules:/root/node_modules \
    -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

## FAQ

### How do I handle npm dependencies?

If you have external dependencies besides `@erpc-cloud/config`, you need to make sure they are available in the container. You can either bundle them into a single JavaScript file using a bundler (e.g., Bun, `tsup`, `terser`) or copy the `node_modules` directory directly into the container. Bundling is recommended for smaller image sizes and better performance.

