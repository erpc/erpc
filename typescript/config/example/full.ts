import { initErpcConfig } from "../dist/index.js";

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
  })
  // Add the two upstreams in our builder store
  .decorate({
    scope: "upstreams",
    value: {
      main: {
        endpoint: "http://localhost:3000",
        rateLimitBudget: "mainUpstreamBudget",
      },
      archive: {
        endpoint: "http://localhost:3001",
        rateLimitBudget: "logUpstreamBudget",
      },
    },
  })
  // Create the project for an indexer
  .addProject(({ store: { upstreams } }) => ({
    id: "indexing",
    upstreams: [upstreams.archive, upstreams.main],
  }))
  // Create a project for a frontend
  .addProject(({ store: { upstreams } }) => ({
    id: "client",
    upstreams: [upstreams.main],
    rateLimitBudget: "frontendBudget",
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
