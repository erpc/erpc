# eRPC

[![CI status][ci-badge]][ci-url]
[![Telegram chat][tg-badge]][tg-url]

[eRPC](https://erpc.cloud/) is a fault-tolerant EVM RPC proxy and re-org aware permanent caching solution. It is built with read-heavy use-cases in mind such as data indexing and high-load frontend usage.

# Features

✅ Retries, circuit-breakers, failovers and hedged requests make sure fastest most-reliable upstream is used. <br/>
✅ Define hourly, daily rate limits for each upstream provider, to control usage, costs and high-scale usage.<br/>
✅ Avoid redundant upstream costs by locally caching RPC responses, with reorg-aware caching layer.<br/>
✅ You don't need to think about which upstream supports which `eth_*` method; eRPC automatically does that.<br/>
✅ Receive consistent error codes with details across 5+ third-party providers and reporting of occured errors.<br/>
✅ Single dashboard to observe rps throughput, errors, and avg. latency of all your RPC providers.<br/>
🏭 Aggregates multiple RPC or contract calls into one.<br/>
🏭 For new blocks and logs load-balanced across upstreams.<br/>

## Roadmap

- Join [eRPC's Telegram](https://t.me/+eEik0_G1VMhmN2U8) for technical discussions and feedbacks.
- Request a feature in [Featurebase](https://erpc.featurebase.app)

### Disclaimer

> ⚠️ This project is still under development, and for now should be used as "a fallback" for RPC calls.

![Architecture](./assets/hla-diagram.svg)

# Usage & Docs

- [docs.erpc.cloud](https://docs.erpc.cloud)

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

AGPL-3.0

[ci-badge]: https://github.com/erpc/erpc/actions/workflows/development.yml/badge.svg
[ci-url]: https://github.com/erpc/erpc/actions/workflows/development.yml
[tg-badge]: https://img.shields.io/endpoint?color=neon&logo=telegram&label=Chat&url=https%3A%2F%2Fmogyo.ro%2Fquart-apis%2Ftgmembercount%3Fchat_id%3Derpc_cloud
[tg-url]: https://t.me/erpc_cloud
[license-badge]: https://img.shields.io/github/license/erpc/erpc
[license-url]: https://github.com/erpc/erpc/blob/main/LICENSE
[version-badge]: https://img.shields.io/github/version/erpc/erpc
[version-url]: https://github.com/erpc/erpc/releases
