<p align="center">
  <img src="https://i.imgur.com/sa4MhlS.png" alt="eRPC Hero" />
</p>

[![Build Status](https://img.shields.io/github/actions/workflow/status/erpc/erpc/release.yml?branch=main&style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/actions/workflows/release.yml)
[![Docs](https://img.shields.io/badge/docs-get%20started-brightgreen?style=flat&colorA=000000&colorB=000000)](https://docs.erpc.cloud/)
[![License](https://img.shields.io/github/license/erpc/erpc?style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/blob/main/LICENSE)
[![Contributors](https://img.shields.io/github/contributors/erpc/erpc?style=flat&colorA=000000&colorB=000000)](https://github.com/erpc/erpc/graphs/contributors)
[![Telegram](https://img.shields.io/endpoint?logo=telegram&url=https%3A%2F%2Fmogyo.ro%2Fquart-apis%2Ftgmembercount%3Fchat_id%3Derpc_cloud&style=flat&colorA=000000&colorB=000000)](https://t.me/erpc_cloud)


[eRPC](https://erpc.cloud/) is a fault-tolerant EVM RPC proxy and **re-org aware permanent caching solution**. It is built with read-heavy use-cases in mind, such as data indexing and high-load frontend usage.

<img src="./assets/hla-diagram.svg" alt="Architecture Diagram" width="70%" />

> **⚠️ Note**: eRPC is still under development. We recommend using it for testnets or as a fallback provider for production RPC calls.

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

---

### Case Studies

- 🔵 [Moonwell: How eRPC slashed RPC calls by 67%](https://erpc.cloud/case-studies/moonwell)  
- 🟢 [Chronicle: How eRPC reduced RPC cost by 45%](https://erpc.cloud/case-studies/chronicle)

---

### Getting Started

<img src="https://i.imgur.com/hCdjkvk.png" alt="Getting Started" width="70%" />

* For full usage instructions and configuration guides, visit [docs.erpc.cloud](https://docs.erpc.cloud).  
* You can also join our [Telegram chat](https://t.me/+eEik0_G1VMhmN2U8) for technical discussions or request features in our [Featurebase](https://erpc.featurebase.app).

---

### Local Development

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
---

### Contributors

<a href="https://github.com/erpc/erpc/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=erpc/erpc&max=50&columns=10&anon=1" alt="Contributors" />
</a>
<br />
<br />
<p>
  By contributing to this project, you agree that your contributions may be used in both the open-source and enterprise versions of the software. Please review our 
  <a href="./CONTRIBUTING.md">Contributing Guidelines</a> and 
  <a href="./CLA.md">Contributor License Agreement</a> before submitting your contributions.
</p>

---

### License

Apache 2.0
