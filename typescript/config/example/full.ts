import { initErpcConfig } from "../src";

/**
 * More complexe example with:
 *  - 3 rate limiters (one for the app, two for the upstream)
 *  - 2 upstreams decorated inside the builder
 *  - 2 project, one for an indexer and one for client
 */
export default initErpcConfig({
  logLevel: "info",
})
  .addRateLimiters({
    // Rate limiter budget for the porject
    frontendBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 10_000,
      },
    ],
    // Rate limiter for the main upstream
    mainUpstreamBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 600,
      },
    ],
    // Rate limiter for the archive upstream
    logUpstreamBudget: [
      {
        method: "eth_getLogs",
        period: "1s",
        waitTime: "1m",
        maxCount: 100,
      },
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 1000,
      },
    ],
    // Rate limiter for the mainnet network
    mainnetBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "5s",
        maxCount: 1000,
      },
    ],
    // Rate limiter for the testnet network
    testnetBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 100,
      },
    ],
  })
  // Add the two upstreams in our builder store
  .decorate("upstreams", {
    main: {
      endpoint: "http://localhost:3000",
      rateLimitBudget: "mainUpstreamBudget",
    },
    archive: {
      endpoint: "http://localhost:3001",
      rateLimitBudget: "logUpstreamBudget",
    },
  })
  // Add our two networks in our builder store
  .decorate("networks", {
    eth: {
      architecture: "evm",
      rateLimitBudget: "mainnetBudget",
      evm: {
        chainId: 1,
      },
    },
    ethSepolia: {
      architecture: "evm",
      rateLimitBudget: "testnetBudget",
      evm: {
        chainId: 11155111,
      },
    },
  })
  // Create the project for an indexer
  .addProject(({ store: { upstreams, networks } }) => ({
    id: "indexing",
    upstreams: [upstreams.archive, upstreams.main],
    networks: [networks.eth, networks.ethSepolia],
  }))
  // Create a project for a frontend
  .addProject(({ store: { upstreams, networks } }) => ({
    id: "client",
    upstreams: [upstreams.main],
    networks: [networks.eth],
    rateLimitBudget: "frontendBudget",
    cors: {
      allowedOrigins: ["mydomain.com"],
      allowedMethods: ["GET", "POST", "OPTIONS"],
      allowedHeaders: ["Content-Type", "Authorization"],
      exposedHeaders: ["X-Request-ID"],
      allowCredentials: true,
      maxAge: 3600,
    },
  }))
  // Create a project for a frontend
  .addProject(({ store: { upstreams, networks } }) => ({
    id: "client-dev",
    upstreams: [upstreams.main],
    networks: [networks.ethSepolia],
    cors: {
      allowedOrigins: ["*"],
      allowedMethods: ["GET", "POST", "OPTIONS"],
      allowedHeaders: ["Content-Type", "Authorization"],
      exposedHeaders: ["X-Request-ID"],
      allowCredentials: true,
      maxAge: 3600,
    },
  }))
  .build();
