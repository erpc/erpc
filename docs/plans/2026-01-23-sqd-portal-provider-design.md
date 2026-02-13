# SQD Portal Provider Design

**Date:** 2026-01-23
**Status:** Draft

## Overview

Add SQD Portal as an upstream provider in eRPC. Portal is a bulk streaming API for historical and real-time blockchain data, supporting blocks, logs, transactions, traces, and state diffs across 100+ EVM networks.

This provider translates standard EVM JSON-RPC methods into Portal queries, allowing SQD Portal to work as a drop-in upstream replacement for read-only historical data queries.

## Configuration

Minimal configuration - just specify the vendor type:

```yaml
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
        upstreams:
          - id: sqd-portal
            type: sqd
```

The provider:
- Detects `type: sqd`
- Looks up chainId → dataset name from hardcoded mapping
- Constructs endpoint: `https://portal.sqd.dev/datasets/{dataset}/stream`

### Supported Networks

Hardcoded chainId → dataset mapping for popular chains:

| ChainId | Dataset Name |
|---------|--------------|
| 1 | ethereum-mainnet |
| 10 | optimism-mainnet |
| 56 | binance-mainnet |
| 137 | polygon-mainnet |
| 8453 | base-mainnet |
| 42161 | arbitrum-one |
| 43114 | avalanche-mainnet |
| ... | (additional chains as needed) |

Unknown chainIds return an error listing supported chains.

## Supported Methods

### Supported (Portal has the data)

| RPC Method | Portal Query | Notes |
|------------|--------------|-------|
| `eth_blockNumber` | `GET /head` | Returns highest block number |
| `eth_getBlockByNumber` | `POST /stream` with block range | Single block, all fields |
| `eth_getBlockByHash` | `POST /stream` + post-filter | Filter by block hash |
| `eth_getLogs` | `POST /stream` with log filters | Address + topics filtering |
| `eth_getTransactionByHash` | `POST /stream` + filter | May need block hint |
| `eth_getTransactionByBlockHashAndIndex` | `POST /stream` + index | Query block, extract tx |
| `eth_getTransactionByBlockNumberAndIndex` | `POST /stream` + index | Query block, extract tx |
| `eth_getTransactionReceipt` | `POST /stream` with tx data | Construct from tx + logs |
| `trace_block` | `POST /stream` with traces | Full block traces |
| `trace_transaction` | `POST /stream` + filter | Single tx traces |
| `debug_traceTransaction` | `POST /stream` + filter | Same as trace_transaction |
| `debug_traceBlockByNumber` | `POST /stream` with traces | Full block traces |
| `debug_traceBlockByHash` | `POST /stream` with traces | Full block traces |

### Unsupported (routed to other upstreams)

- `eth_call`, `eth_estimateGas` - require EVM execution
- `eth_sendRawTransaction` - requires mempool access
- `eth_getBalance`, `eth_getCode`, `eth_getStorageAt`, `eth_getTransactionCount` - require state access

## Request Translation

### Example: `eth_getLogs` → Portal Query

**RPC Request:**
```json
{
  "method": "eth_getLogs",
  "params": [{
    "fromBlock": "0x112A880",
    "toBlock": "0x112A88F",
    "address": "0xdAC17F958D2ee523a2206206994597C13D831ec7",
    "topics": ["0xddf252ad..."]
  }]
}
```

**Portal Request:**
```json
{
  "type": "evm",
  "fromBlock": 18000000,
  "toBlock": 18000015,
  "fields": {
    "log": {
      "logIndex": true,
      "transactionIndex": true,
      "transactionHash": true,
      "address": true,
      "data": true,
      "topics": true
    },
    "block": {
      "number": true,
      "hash": true
    }
  },
  "logs": [{
    "address": ["0xdAC17F958D2ee523a2206206994597C13D831ec7"],
    "topic0": ["0xddf252ad..."]
  }]
}
```

### Example: `eth_getBlockByNumber` → Portal Query

**RPC Request:**
```json
{
  "method": "eth_getBlockByNumber",
  "params": ["0x112A880", true]
}
```

**Portal Request:**
```json
{
  "type": "evm",
  "fromBlock": 18000000,
  "toBlock": 18000000,
  "fields": {
    "block": {
      "number": true,
      "hash": true,
      "parentHash": true,
      "timestamp": true,
      "miner": true,
      "gasUsed": true,
      "gasLimit": true,
      "baseFeePerGas": true
    },
    "transaction": {
      "hash": true,
      "from": true,
      "to": true,
      "value": true,
      "input": true,
      "gas": true,
      "gasPrice": true
    }
  }
}
```

### Response Handling

- Portal returns NDJSON (newline-delimited JSON)
- Parse streaming response, collect all lines
- Transform to standard JSON-RPC response format
- For single-item queries (getBlock, getTx), return first match

## Implementation Architecture

### New Files

```
thirdparty/
  sqd.go                    # Vendor implementation
  sqd_chain_mapping.go      # ChainId → dataset mapping

clients/
  sqd_portal_client.go      # HTTP client for Portal API (NDJSON handling)
```

### Key Components

#### SqdVendor (`thirdparty/sqd.go`)

Implements `common.Vendor` interface:
- `Name()` → `"sqd"`
- `OwnsUpstream()` → checks `type: sqd`
- `GenerateConfigs()` → looks up chainId, sets endpoint URL
- `GetVendorSpecificErrorIfAny()` → handles Portal error codes (429, 503, etc.)

#### SqdPortalClient (`clients/sqd_portal_client.go`)

New client type implementing `ClientInterface`:
- Handles NDJSON streaming responses
- Method translation logic (RPC → Portal query)
- Response transformation (Portal → JSON-RPC)

#### Chain Mapping (`thirdparty/sqd_chain_mapping.go`)

```go
var SqdChainToDataset = map[int64]string{
    1:     "ethereum-mainnet",
    10:    "optimism-mainnet",
    56:    "binance-mainnet",
    137:   "polygon-mainnet",
    8453:  "base-mainnet",
    42161: "arbitrum-one",
    43114: "avalanche-mainnet",
    // ... additional chains
}
```

### Integration Points

- Register vendor in `thirdparty/vendors_registry.go`
- Register client type in `clients/registry.go`
- Add `ClientTypeSqdPortal` constant

## Error Handling

### Portal Error Codes → eRPC Handling

| Portal Status | Meaning | eRPC Action |
|---------------|---------|-------------|
| 200 | Success | Parse NDJSON, return result |
| 204 | No data in range | Return empty result ([], null) |
| 400 | Invalid query | Return JSON-RPC error (-32602) |
| 404 | Dataset not found | Return JSON-RPC error, log warning |
| 409 | Chain reorg conflict | Retry with updated block hash |
| 429 | Rate limited | Respect `Retry-After`, trigger circuit breaker |
| 503 | Service unavailable | Retry with backoff |

### Edge Cases

1. **Block not yet indexed** - If requested block > `/head`, return null and let eRPC route to another upstream.

2. **Large log ranges** - Set reasonable timeout (30s), let eRPC's failsafe handle retries.

3. **Transaction not found** - Return null if not in reasonable recent range, let other upstreams handle.

4. **Finality awareness** - Use `/finalized-stream` for finalized requests, `/stream` for latest.

5. **Empty responses** - Portal 204 → appropriate empty response ([], null) based on method.

## Testing Strategy

### Unit Tests

- ChainId → dataset mapping
- RPC → Portal query translation for each method
- Portal response → JSON-RPC response transformation
- Error code handling

### Integration Tests

Against real Portal (public tier):
- `eth_blockNumber` returns valid block
- `eth_getBlockByNumber` returns correct block structure
- `eth_getLogs` with known contract returns expected logs
- Rate limit handling (trigger 429, verify backoff)

## Out of Scope (Initial Implementation)

- API key authentication (add later via `vendorSettings.apiKey`)
- State diff methods (custom `sqd_getStateDiffs` method)
- Prefetch/batch optimization
- WebSocket subscriptions
- Custom field selection configuration
- Self-hosted Portal instances

## Future Enhancements

1. Add `vendorSettings.apiKey` for Cloud Portal (higher rate limits)
2. Add `sqd_getStateDiffs` custom method to expose state diff data
3. Prefetch nearby blocks for sequential access patterns
4. Support for self-hosted Portal instances via custom endpoint config
