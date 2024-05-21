## Principles

- Minimum user configuration:
    - Automatically detect/infer as much as possible
    - Sane defaults
    - Prepopulated config templates based on current public providers info

## Components

### Core

#### Config

```yaml
logLevel: DEBUG | INFO | WARN | ERROR

server:
  httpPort: number
  websocketPort: number
  maxTimeoutMs: mumber | string

metrics:
  port: number

store:
  driver: memory | redis | postgresql | dynamodb
  memory:
    maxSize: string
  redis:
    host: string
    port: number
    password: string
  postgresql:
    host: string
    port: number
    user: string
    password: string
    database: string
  dynamodb:
    region: string
    endpoint: string
    table: string
    auth:
      mode: env | file | secret
      credentialsFile: string
      profile: string
      accessKeyID: string
      secretAccessKey: string
  
projects:
  - id: string # main, frontend, sushiswap-prod, etc ...
    rateLimitBucket: string

	  # Define as many upstreams for this project
    upstreams:
    - id: string
      architecture: auto | evm | substrate | solana | bitcoin
      endpoint: string # wss:// or wss+alchemy://MY_ALCHEMY_API_KEY or https+alchemy://MY_ALCHEMY_API_KEY?chainId=1,5
      headers:
        key: value
      metadata:
        evmChainId: auto | number
        evmNodeType: auto | full | archive
        evmGetLogsMaxRange: auto | number
        evmReceiptsMode: auto | eth | parity | alchemy
      supportedMethods: [string] # supports wildcard: eth_*  parity_*  alchemy_* 
      unsupportedMethods: [string]
      creditUnitMapping: string # e.g. alchemy-growth-plan
      maxBatchSize: number
      rateLimitBucket: string
      healthCheckGroup: string
      failsafe:
        timeout:
          duration: number | string
        retry:
          maxCount: number
          delay: number | string
          backoffMaxDelay: number | string
          backoffFactor: number
          jitter: number | string
        hedge:
          delay: number | string
          maxCount: number
        circuitBreaker:
          failureThresholdCount: number
          failureThresholdCapacity: number
          halfOpenAfter: number | string
          successThresholdCount: number
          successThresholdCapacity: number

	  # Optionally provide per-chain configs
    networks:
    - architecture: evm
      networkId: number
      rateLimitBucket: string
      failsafe:
        retry:
          maxCount: number
          delay: number | string
          backoffMaxDelay: number | string
          backoffFactor: number
          jitter: number | string
        circuitBreaker:
          failureThresholdCount: number
          failureThresholdCapacity: number
          halfOpenAfter: number | string
          successThresholdCount: number
          successThresholdCapacity: number
        timeout:
          duration: number | string
        hedge:
          delay: number | string
          maxCount: number
      prefetch:
        enabled: boolean
        historicalSync: true
        realtimeSync: true
        evmBlocks: true
        evmTransactions: true
        evmTraces: true
        evmEvents:
	        filters: [] # same as getLogs: [address, topic0, ...]
		
rateLimiters:
  buckets:
  - id: string
    rules:
    - method: string | *
      maxCount: number
      period: number | string
      waitTime: number | string
      scope: instance | cluster
    
healthChecks:
  groups:
  - id: string
    checkInterval: number | string
    maxErrorRatePercent: number
    maxP90LatencyMs: mumber | string
    maxBlocksBehind: number

# Useful mappings you can re-use on multiple upstreams
creditUnitMappings:
  - id: string
    methods:
    - name: string | string* | *
      value: number
```

### Proxy

* **HttpServer**
    - Listens to port 3000
    - Handles the telemetry and request/response encoding/decoding
    - Resolving the project and metadata (chainId) from the request (either in path, or in headers, or in body)
    - Calls the ProxyCore with the RPC request and expects a normalized response

* **ProxyCore**
    - Calls RequestNormalizer to normalize the request to prepare the final actual request body (resolving "latest" to an actual block, or resolving the correct receipts method name, etc.)
    - Calls DAL check if it is cached already
    - Calls UpstreamsRegistry to get the best upstream based on the project and metadata and request
    - Calls the upstream with the actual request body and normalize the response / errors via ResponseNormalizer
        - Reports to HealthCenter with endpoint performance and/or errors
        - Reports to RateLimitService with the usage of upstream
    - Calls DAL to store in the cache if applicable
    - Return the response to the HttpServer
    
* **DAL**
    - Abstracts certain high-level operations such as checking if a certain getLogs request is fully cached or calculate remaining blocks/address/topics to fetch

* **DataStore**
    - Interacts with underlying storage engine with unified methods (set, get, delete, scan)

* **HealthCenter**
    - Tracks the health of the final endpoints
        - Rate of failures
        - P90 latency
        - Upstream availability (circuit breaker)
    - Provides the health status to the UpstreamsRegistry per group configuration
    - Periodically syncs the health via DAL for other instances

* **RateLimitService**
    - Tracks usage for each rate limit group
    - Provides info about the current usage (e.g. least busy member, etc.)

* **UpstreamsRegistry**:
    - Queries all upstreams from data store (which is initialized by the config) for the project
    - Pick the best one based on reports from HealthCenter and RateLimitService regarding the health and usage of the upstreams (weight, rate limits, health, etc.)
    - Periodically sorts the upstreams based on:
        - User-defined weight
        - User-defined rate limits
            - safe: remaining > 10%
            - close: remaining < 10%
            - breach: remaining < 1%
        - Health status:
            - healthy: no errors in the last 5 minutes
            - unhealthy: more than 10% errors in the last 5 minutes
            - dead: more than 50% errors in the last 5 minutes
    - Proposes the best upstream to the ProxyCore based on the project and metadata and request
        - User-defined routing
        - Supported methods (declarted and/or automatically inferred?)
        - Inferred routing (e.g. detect archive nodes)

* **UpstreamFeatureDetector**
    - Detects the features of the upstreams (e.g. chainId, archive node, getLogs max range, etc.)
