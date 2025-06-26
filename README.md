<p align="center">
  <img src="https://i.imgur.com/sa4MhlS.png" alt="eRPC Hero" />
</p>

[![Build Status](https://img.shields.io/github/actions/workflow/status/erpc/erpc/release.yml?branch=main&style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/actions/workflows/release.yml)
[![Docs](https://img.shields.io/badge/docs-get%20started-brightgreen?style=flat&colorA=000000&colorB=000000)](https://docs.erpc.cloud/)
[![License](https://img.shields.io/github/license/erpc/erpc?style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/blob/main/LICENSE)
[![Contributors](https://img.shields.io/github/contributors/erpc/erpc?style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/graphs/contributors)
[![Telegram](https://img.shields.io/endpoint?logo=telegram&url=https%3A%2F%2Ftg.sumanjay.workers.dev%2Ferpc_cloud&style=flat&colorA=000000&colorB=000000&label=telegram)](https://t.me/erpc_cloud)

eRPC is a fault-tolerant EVM RPC proxy and **re-org aware permanent caching solution**. It is built with read-heavy use-cases in mind, such as data indexing and high-load frontend usage.

<img src="./assets/hla-diagram.svg" alt="Architecture Diagram" width="70%" />

> **⚠️ Note**: eRPC is still under development. We recommend using it for testnets or as a fallback provider for production RPC calls.

---

### Quick Start

With the below setup, you get immediate access to 2,000+ chains and 4,000+ public free EVM RPC endpoints.

#### Run an eRPC instance:

Using `npx`:

```bash
npx start-erpc
```

Or, using `Docker`:

```bash
docker run -p 4000:4000 ghcr.io/erpc/erpc
```

Or, using `Railway`:

[![Deploy on Railway](https://railway.app/button.svg)](https://railway.com/template/10iW1q?referralCode=PpPFJd)

#### Send a request to your eRPC instance:

```bash
curl 'http://localhost:4000/main/evm/42161' \
--header 'Content-Type: application/json' \
--data '{
    "method": "eth_getBlockByNumber",
    "params": ["latest", false],
    "id": 9199,
    "jsonrpc": "2.0"
}'
```

#### Next Steps:
This setup is ideal for development and testing purposes. For production environments, we recommend extending your configuration with dedicated premium providers and advanced failover settings. See our [Configuration Guide](https://docs.erpc.cloud/config/example) for more details.

---

### Key Features

- **Retries, circuit breakers, failovers, and hedged requests**: Ensures the fastest, most reliable upstream is always used  
- **Configurable rate limits**: Set hourly or daily [rate limits](https://docs.erpc.cloud/config/rate-limiters) per upstream to control usage and costs  
- **Local re-org aware cache**: Avoid redundant upstream calls and maintain data consistency when blockchain reorgs occur  
- **Automatic method routing**: No need to worry which provider supports which `eth_*` method  
- **Unified error handling**: Consistent error codes and detailed messages across multiple providers  
- **Single dashboard**: Observe throughput (RPS), errors, and average latency across all providers  
- **Flexible authentication**: Supports [basic auth, secrets, JWT, SIWE](https://docs.erpc.cloud/config/auth) and more  
- **Smart batching**: [Aggregate multiple RPC or contract calls into one](https://docs.erpc.cloud/operation/batch)
- **Selection policy**: Allows you to influence how upstreams are selected to serve traffic (or not) w/ [selection policies](https://docs.erpc.cloud/config/projects/selection-policies).
- **Data integrity**: Ensures accurate, up-to-date responses by levering several [data integrity mechanisms](https://docs.erpc.cloud/config/failsafe/integrity).
- **Consensus policy**: Compares results from multiple upstreams and punishes nodes that consistently disagree.

---

### Case Studies

- 🔵 [Moonwell: How eRPC slashed RPC calls by 67%](https://erpc.cloud/case-studies/moonwell)  
- 🟢 [Chronicle: How eRPC reduced RPC cost by 45%](https://erpc.cloud/case-studies/chronicle)

---

### Local Development

1. **Clone this repository:**

```bash
git clone https://github.com/erpc/erpc.git
```

2. **Install Go dependencies:**

```bash
make setup
```

3. **Create a configuration file:**

```bash
cp erpc.dist.yaml erpc.yaml
vi erpc.yaml
```

4. **Run the eRPC server:**

```bash
make run
```

---

### Contributors

<a href="https://github.com/erpc/erpc/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=erpc/erpc&max=50&columns=10&anon=1" alt="Contributors" />
</a>

<p>
  By contributing to this project, you agree that your contributions may be used in both the open-source and enterprise versions of the software. Please review our 
  <a href="./CONTRIBUTING.md">Contributing Guidelines</a> and 
  <a href="./CLA.md">Contributor License Agreement</a> before submitting your contributions.
</p>

---

### License

Apache 2.0 
