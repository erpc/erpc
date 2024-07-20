# eRPC

[![CI status][ci-badge]][ci-url]
[![Telegram chat][tg-badge]][tg-url]

Open-source EVM RPC proxy & cache service built to scale horizontally from small traffic to million RPS across many chains, optimized for read-heavy use-cases such as Indexers, Frontends, MEV bots, etc.

## Roadmap

- Join [eRPC's Telegram](https://t.me/+eEik0_G1VMhmN2U8) for technical discussions and feedbacks.
- Request a feature in [Featurebase](https://erpc.featurebase.app)

### Disclaimer

> ⚠️ This project is still under development, and for now should be used as "a fallback" for RPC calls.

![Architecture](./assets/hla-diagram.svg)

# Usage & Docs

* [docs.erpc.cloud](https://docs.erpc.cloud)

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

[ci-badge]: https://github.com/erpc/erpc/actions/workflows/development.yml/badge.svg
[ci-url]: https://github.com/erpc/erpc/actions/workflows/development.yml
[tg-badge]: https://img.shields.io/endpoint?color=neon&logo=telegram&label=Chat&url=https%3A%2F%2Fmogyo.ro%2Fquart-apis%2Ftgmembercount%3Fchat_id%3Derpc_cloud
[tg-url]: https://t.me/erpc_cloud
[license-badge]: https://img.shields.io/github/license/erpc/erpc
[license-url]: https://github.com/erpc/erpc/blob/main/LICENSE
[version-badge]: https://img.shields.io/github/version/erpc/erpc
[version-url]: https://github.com/erpc/erpc/releases
