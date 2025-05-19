import http from 'k6/http';
import { check, randomSeed } from 'k6';
import { Rate, Counter, Trend, Gauge } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.1.0/index.js';

// Target URL (can be configured via environment variables)
const ERPC_BASE_URL = __ENV.ERPC_BASE_URL || 'http://localhost:4000/main/evm/';

// Common ERC20 Transfer event topic
const TRANSFER_EVENT_TOPIC = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';

const CHAINS = {
  // ETH: {
  //   id: '1',
  //   blockMin: 0x1006F40,
  //   blockMax: 0x1406F40,
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     blockHashSamplePool: []
  //   }
  // },
  // ABS: {
  //   id: '11124',
  //   blockMin: 0x194920,
  //   blockMax: 0x694920,
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     blockHashSamplePool: []
  //   }
  // },
  // POLYGON: {
  //   id: '137',
  //   blockMin: 0x20D9900,
  //   blockMax: 0x40D9900,
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     blockHashSamplePool: []
  //   }
  // },
  // ARBITRUM: {
  //   id: '42161',
  //   blockMin: 0x10E1A300,
  //   blockMax: 0x11E1A300,
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     blockHashSamplePool: []
  //   }
  // },
  BASE: {
    id: '8453',
    blockMin: 0x1312D00,
    blockMax: 0x1D05E9C,
    cached: {
      latestBlock: null,
      latestBlockTimestamp: 0,
      blockHashSamplePool: []
    }
  }
};

if (__ENV.RANDOM_SEED) {
  randomSeed(parseInt(__ENV.RANDOM_SEED));
}

// Traffic pattern weights (in percentage, should sum to 100)
const TRAFFIC_PATTERNS = {
  RANDOM_HISTORICAL_BLOCKS: 0,      // Fetch random blocks from history
  RANDOM_LOG_RANGES: 30,             // Get logs for random block ranges
  RANDOM_HISTORICAL_RECEIPTS: 0,    // Get random transaction receipts from history
  RANDOM_TRACE_TRANSACTIONS: 0,     // Trace random transactions with various methods
  RANDOM_BLOCK_BY_HASH: 70,         // Get random block by hash
};

// Configuration
const CONFIG = {   
  LOG_RANGE_MIN_BLOCKS: 1,
  LOG_RANGE_MAX_BLOCKS: 100,
  LATEST_BLOCK_CACHE_TTL: 5,         // seconds
};

// K6 configuration
export const options = {
  scenarios: {    
    constant_request_rate: {
      executor: 'constant-arrival-rate',
      rate: 300,
      timeUnit: '1s',
      duration: '30m',
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
  },
};

const statusCodeCounter = new Counter('status_codes');
const jsonRpcErrorCounter = new Counter('jsonrpc_errors');
const parsingErrorsCounter = new Counter('parsing_errors');
const responseSizes = new Trend('response_sizes');

function getRandomChain() {
  const chains = Object.values(CHAINS);
  return chains[randomIntBetween(0, chains.length - 1)];
}

function getRandomBlock(chain) {
  return `0x${randomIntBetween(chain.blockMin, chain.blockMax).toString(16)}`;
}

function getRandomBlockRange(chain) {
  const fromBlock = parseInt(getRandomBlock(chain), 16);
  const rangeSize = randomIntBetween(CONFIG.LOG_RANGE_MIN_BLOCKS, CONFIG.LOG_RANGE_MAX_BLOCKS);
  return {
    fromBlock: `0x${fromBlock.toString(16)}`,
    toBlock: `0x${(fromBlock + rangeSize).toString(16)}`
  };
}

function randomHistoricalBlocks(http, params, chain) {
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(chain), false],
    id: Math.floor(Math.random() * 100000000)
  });
  return http.post(ERPC_BASE_URL + chain.id, payload, params);
}

function randomBlockByHash(http, params, chain) {
  if (!chain.cached.blockHashSamplePool.length) {
    return
  }
  const selectedHash = chain.cached.blockHashSamplePool[randomIntBetween(0, chain.cached.blockHashSamplePool.length - 1)];
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByHash",
    params: [selectedHash, true],  
    id: Math.floor(Math.random() * 100000000)
  });
  return http.post(ERPC_BASE_URL + chain.id, payload, params);
}

function randomLogRanges(http, params, chain) {
  const { fromBlock, toBlock } = getRandomBlockRange(chain);
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getLogs",
    params: [{
      fromBlock,
      toBlock,
      topics: [TRANSFER_EVENT_TOPIC]
    }],
    id: Math.floor(Math.random() * 100000000)
  });
  const res = http.post(ERPC_BASE_URL + chain.id, payload, params);
  if (res.status < 300) {
    try {
      const body = JSON.parse(res.body);
      if (body && body.result && body.result.length > 0) {
        for (const log of body.result) {
          if (chain.cached.blockHashSamplePool.indexOf(log.blockHash) === -1) {
            console.debug(`Adding block hash to sample pool: ${log.blockHash}`);
            chain.cached.blockHashSamplePool.push(log.blockHash);
          }
        }
      }
    } catch(e){}
  }
  return res;
}

function randomHistoricalReceipts(http, params, chain) {
  // TODO maybe find a way to gather bunch of random transaction hashes before the test starts to avoid this extra call PER request
  const blockPayload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(chain), true],
    id: Math.floor(Math.random() * 100000000)
  });
  
  const blockRes = http.post(ERPC_BASE_URL + chain.id, blockPayload, params);
  if (blockRes.status !== 200) return blockRes;

  try {
    const block = JSON.parse(blockRes.body);
    if (!block.result || !block.result.transactions || block.result.transactions.length === 0) {
      return blockRes;
    }

    // Get a random transaction from the block
    const tx = block.result.transactions[randomIntBetween(0, block.result.transactions.length - 1)];
    const receiptPayload = JSON.stringify({
      jsonrpc: "2.0",
      method: "eth_getTransactionReceipt",
      params: [tx.hash],
      id: Math.floor(Math.random() * 100000000)
    });
    return http.post(ERPC_BASE_URL + chain.id, receiptPayload, params);
  } catch (e) {
    console.error(`Failed to process block response: ${e}`);
    return blockRes;
  }
}

async function traceRandomTransaction(http, params, chain) {
  // First get a random block with transactions
  const blockPayload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(chain), true],
    id: Math.floor(Math.random() * 100000000)
  });
  
  const blockRes = await http.post(ERPC_BASE_URL + chain.id, blockPayload, params);
  if (blockRes.status !== 200) return blockRes;

  try {
    const block = JSON.parse(blockRes.body);
    if (!block.result || !block.result.transactions || block.result.transactions.length === 0) {
      return blockRes;
    }

    // Get a random transaction from the block
    const tx = block.result.transactions[randomIntBetween(0, block.result.transactions.length - 1)];
    
    // Standard Ethereum trace methods
    const traceMethods = [
      {
        method: "debug_traceTransaction",
        params: [tx.hash, { tracer: "callTracer" }]
      },
      {
        method: "trace_replayTransaction",
        params: [tx.hash, ["trace"]]
      },
      {
        method: "trace_transaction",
        params: [tx.hash]
      }
    ];

    // Try each trace method until one succeeds
    for (const traceMethod of traceMethods) {
      const tracePayload = JSON.stringify({
        jsonrpc: "2.0",
        method: traceMethod.method,
        params: traceMethod.params,
        id: Math.floor(Math.random() * 100000000)
      });

      const traceRes = await http.post(ERPC_BASE_URL + chain.id, tracePayload, params);
      if (traceRes.status === 200) {
        try {
          const body = JSON.parse(traceRes.body);
          if (body.result && !body.error) {
            return traceRes;
          }
        } catch (e) {
          console.error(`Failed to parse trace response: ${e}`);
        }
      }
    }

    // If all trace methods fail, return the last response
    return blockRes;
  } catch (e) {
    console.error(`Failed to process block or trace response: ${e}`);
    return blockRes;
  }
}

function randomIntBetween(min, max) {
  return Math.floor(Math.random() * (max - min + 1)) + min;
}

// Main test function
export default async function () {
  const params = {
    headers: { 'Content-Type': 'application/json' },
    insecureSkipTLSVerify: true,
    timeout: '30s',
  };

  // Randomly select traffic pattern based on weights
  const selectedChain = getRandomChain();
  const tags = { chain: selectedChain.id };  
  const rand = Math.random() * 100;
  let cumulativeWeight = 0;
  let res;

  for (const [pattern, weight] of Object.entries(TRAFFIC_PATTERNS)) {
    cumulativeWeight += weight;
    if (rand <= cumulativeWeight) {
      tags['pattern'] = pattern;
      params['tags'] = tags;
      switch (pattern) {
        case 'RANDOM_HISTORICAL_BLOCKS':
          res = randomHistoricalBlocks(http, params, selectedChain);
          break;
        case 'RANDOM_LOG_RANGES':
          res = randomLogRanges(http, params, selectedChain);
          break;
        case 'RANDOM_HISTORICAL_RECEIPTS':
          res = randomHistoricalReceipts(http, params, selectedChain);
          break;
        case 'RANDOM_TRACE_TRANSACTIONS':
          res = await traceRandomTransaction(http, params, selectedChain);
          break;
        case 'RANDOM_BLOCK_BY_HASH':
          res = randomBlockByHash(http, params, selectedChain);
          break;
      }
      break;
    }
  }
  
  // const sampleReq = {"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0xfaeff2","toBlock":"0xfaeff3","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}],"id":Math.ceil(Math.random() * 10000000)};
  // res = http.post(ERPC_BASE_URL + selectedChain.id, JSON.stringify(sampleReq), params);

  if (res) {
    tags['status_code'] = res.status.toString();
    statusCodeCounter.add(1, tags);
    responseSizes.add(res.body.length, tags);

    if (__ENV.DEBUG) {
      if (res.status >= 400) {
        console.log(`Request: ${JSON.stringify(sampleReq)} Status Code: ${res.status} Response body: ${res.body} Tags: ${JSON.stringify(tags)}`);
      }
    }

    let parsedBody = null;
    try {
      parsedBody = JSON.parse(res.body);
    } catch (e) {
      parsingErrorsCounter.add(1, tags);
      console.error(`Failed to parse response body: ${e} body: ${res.body}`);
    }

    if (parsedBody?.error?.code) {
      tags['rpc_error_code'] = parsedBody.error.code.toString();
      tags['rpc_error_message'] = parsedBody?.error?.message;
      jsonRpcErrorCounter.add(1, tags);
    }

    check(res, {
      'status is 2xx': (r) => r.status >= 200 && r.status < 300,
      'response has no json-rpc error': (r) => {
        if (parsedBody?.error?.code) {
          return false;
        }
        return true;
      },
    }, tags);
  } else {
    console.debug(`skipped as no response for ${tags['pattern']} ${selectedChain.id}`);
  }
}

export function handleSummary(data) {
  const statusCodeStats = {};
  data?.metrics?.status_codes?.values?.tags?.forEach(t => {
    const code = t.status_code;
    statusCodeStats[code] = (statusCodeStats[code] || 0) + t.value;
  });

  const jsonRpcErrorStats = {};
  data?.metrics?.jsonrpc_errors?.values?.tags?.forEach(t => {
    const key = `${t.code}:${t.message}`;
    jsonRpcErrorStats[key] = (jsonRpcErrorStats[key] || 0) + t.value;
  });

  console.log('\nStatus Code Breakdown:', JSON.stringify(statusCodeStats));
  Object.entries(statusCodeStats || {}).forEach(([code, count]) => {
    console.log(`  HTTP ${code}: ${count} requests`);
  });

  console.log('\nJSON-RPC Error Breakdown:', JSON.stringify(jsonRpcErrorStats));
  Object.entries(jsonRpcErrorStats || {}).forEach(([key, count]) => {
    const [code, message] = key.split(':');
    console.log(`  Code ${code} (${message}): ${count} errors`);
  });

  return {
    stdout: textSummary(data, { indent: "  ", enableColors: true }),
  };
}