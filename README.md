# eRPC

[![join chat](https://img.shields.io/badge/Telegram-join%20chat-blue)](https://t.me/+eEik0_G1VMhmN2U8)

Open-source EVM RPC proxy & cache service built to scale horizontally from small traffic to million RPS across many chains, optimized for read-heavy use-cases such as Indexers, Frontends, MEV bots, etc.

## Roadmap

- Join [eRPC's Telegram](https://t.me/+eEik0_G1VMhmN2U8) for technical discussions and feedbacks.
- Request a feature in [Featurebase](https://erpc.featurebase.app)

### Disclaimer

> This project is still under development, and for now should be used as "a fallback" for RPC calls.

![Architecture](./assets/hla-diagram.svg)

# Usage

1. Create your `erpc.yaml` configuration file based on the [`erpc.yaml.dist`](./erpc.yaml.dist) file.

```yaml
# vi ./erpc.yaml
logLevel: warn
database:
  evmJsonRpcCache:
    driver: memory
    # driver: postgresql
    # postgresql:
    #   connectionUri: >-
    #     postgres://YOUR_USERNAME_HERE:YOUR_PASSWORD_HERE@your.postgres.hostname.here.com:5432/your_database_name
    #   table: rpc_cache
server:
  httpHost: 0.0.0.0
  httpPort: 4000
metrics:
  enabled: true
  host: 0.0.0.0
  port: 4001
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
        failsafe:
          timeout:
            duration: 30s
          retry:
            maxCount: 3
            delay: 500ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 500ms
          hedge:
            delay: 3000ms
            maxCount: 2
      - architecture: evm
        evm:
          chainId: 42161
        failsafe:
          timeout:
            duration: 30s
          retry:
            maxCount: 5
            delay: 500ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 200ms
          hedge:
            delay: 1000ms
            maxCount: 2
    upstreams:
      - id: alchemy-multi-chain-example
        endpoint: alchemy://XXXX_YOUR_ALCHEMY_API_KEY_HERE_XXXX
        rateLimitBudget: global
        healthCheckGroup: default-hcg
        allowMethods:
          - "alchemy_*"
        ignoreMethods:
          - "trace_transaction"
        failsafe:
          timeout:
            duration: 15s
          retry:
            maxCount: 2
            delay: 1000ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 500ms
      - id: blastapi-chain-42161
        type: evm
        endpoint: https://arbitrum-one.blastapi.io/xxxxxxx-xxxxxx-xxxxxxx
        rateLimitBudget: global-blast
        healthCheckGroup: default-hcg
        evm:
          chainId: 42161
        allowMethods:
          - "eth_*"
        ignoreMethods:
          - "eth_trace"
        failsafe:
          timeout:
            duration: 15s
          retry:
            maxCount: 2
            delay: 1000ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 500ms
      - id: blastapi-chain-1
        type: evm
        endpoint: https://eth-mainnet.blastapi.io/xxxxxxx-xxxxxx-xxxxxxx
        rateLimitBudget: global-blast
        healthCheckGroup: default-hcg
        evm:
          chainId: 1
        failsafe:
          timeout:
            duration: 15s
          retry:
            maxCount: 2
            delay: 1000ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 500ms
      - id: quiknode-chain-42161
        type: evm
        endpoint: https://xxxxxx-xxxxxx.arbitrum-mainnet.quiknode.pro/xxxxxxxxxxxxxxxxxxxxxxxx/
        rateLimitBudget: global-quicknode
        healthCheckGroup: default-hcg
        evm:
          chainId: 42161
        failsafe:
          timeout:
            duration: 15s
          retry:
            maxCount: 2
            delay: 1000ms
            backoffMaxDelay: 10s
            backoffFactor: 0.3
            jitter: 500ms
rateLimiters:
  budgets:
    - id: default-budget
      rules:
        - method: "*"
          maxCount: 10000
          period: 1s
    - id: global-blast
      rules:
        - method: "*"
          maxCount: 1000
          period: 1s
    - id: global-quicknode
      rules:
        - method: "*"
          maxCount: 300
          period: 1s
healthChecks:
  groups:
    - id: default-hcg
      checkInterval: 30s
      maxErrorRatePercent: 10
      maxP90LatencyMs: 5s
      maxBlocksBehind: 5
```

2. Use the Docker image:

```bash
docker run -v $(pwd)/erpc.yaml:/app/erpc.yaml -p 4000:4000 -p 4001:4001 ghcr.io/erpc/erpc:latest
```

3. Send your first request:

```bash
curl --location 'http://localhost:4000/main/evm/42161' \
--header 'Content-Type: application/json' \
--data '{
    "method": "eth_getBlockByNumber",
    "params": [
        "0x1203319",
        false
    ],
    "id": 9199,
    "jsonrpc": "2.0"
}'
```

4. Bring up monitoring stack (Prometheus, Grafana) using docker-compose:

```bash
# clone the repo if you haven't
git clone https://github.com/erpc/erpc.git
cd erpc

# bring up the monitoring stack
docker-compose up -d
```

5. Open Grafana at [http://localhost:3000](http://localhost:3000) and login with the following credentials:

- username: `admin`
- password: `admin`

6. Send more requests and watch the metrics being collected and visualized in Grafana.

[![eRPC Grafana Dashboard](./assets/monitoring-example-erpc.png)](https://grafana.com/grafana/dashboards/186080)

## Local Development

1. Clone this repository:

```bash
git clone https://github.com/erpc/erpc.git
```

2. Install Go dependencies:

```bash
make setup
```

3. Create a `erpc.yaml` configuration file based on the [`erpc.yaml.dist`](./erpc.yaml.dist) file, and use your RPC provider credentials:

```bash
cp erpc.yaml.dist erpc.yaml
vi erpc.yaml
```

4. Run the eRPC server:

```bash
make run
```

# License

AGPL-3.0 - Free for personal or open-source commercial use

> For a closed-source commercial usage (e.g. selling as a SaaS), please [contact us](https://docs.flair.dev/talk-to-an-engineer).
