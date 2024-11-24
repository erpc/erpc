import { initErpcConfig } from "../dist/index.js";

/**
 * Very simple example, creating a config with:
 *  - 2 rate limiters (one for the app, and one for the upstream)
 *  - 1 project with one upstream
 */
export default initErpcConfig({
  logLevel: "info",
})
  .addRateLimiters({
    // Rate limiter budget for the porject
    projectBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 1000,
      },
    ],
    // Rate limiter for the upstream
    upstreamBudget: [
      {
        method: "*",
        period: "1s",
        waitTime: "1m",
        maxCount: 1000,
      },
    ],
  })
  .addProject({
    // Project with one upstream
    id: "main-project",
    upstreams: [
      {
        id: "upstream-1",
        endpoint: "http://localhost:3000",
        rateLimitBudget: "upstreamBudget",
      },
    ],
    rateLimitBudget: "projectBudget",
  })
  .build();
