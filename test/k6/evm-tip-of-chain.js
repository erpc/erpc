import http from 'k6/http';
import { check, randomSeed } from 'k6';
import { Rate, Counter, Trend, Gauge } from 'k6/metrics';

// Target URL (can be configured via environment variables)
const ERPC_BASE_URL = __ENV.ERPC_BASE_URL || 'http://localhost:4000/main/evm/';

// Traffic pattern weights (in percentage, should sum to 100)
const TRAFFIC_PATTERNS = {
  LATEST_BLOCK_WITH_LOGS: 30,        // Get latest block and its transfer logs
  LATEST_BLOCK_RECEIPTS: 30,         // Get receipts from latest block's transactions
  LATEST_BLOCK_TRACES: 25,           // Get traces from latest block's transactions
  RANDOM_ACCOUNT_BALANCES: 15,       // Get random account balances
};

// Configuration
const CONFIG = {   
  LOG_RANGE_MIN_BLOCKS: 1,
  LOG_RANGE_MAX_BLOCKS: 100,
  LATEST_BLOCK_CACHE_TTL: 1,         // reduced to 1 second
  MAX_CACHED_TXS: 50,                // maximum number of transaction hashes to cache
};

// Update CHAINS structure to include transaction cache
const CHAINS = {
  ETH: {
    id: '1',
    cached: {
      latestBlock: null,
      latestBlockTimestamp: 0,
      transactions: []
    }
  },
  // POLYGON: {
  //   id: '137',
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     transactions: []
  //   }
  // },
  // ARBITRUM: {
  //   id: '42161',
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     transactions: []
  //   }
  // }
};

if (__ENV.RANDOM_SEED) {
  randomSeed(parseInt(__ENV.RANDOM_SEED));
}

// K6 configuration
export const options = {
  scenarios: {    
    constant_request_rate: {
      executor: 'constant-arrival-rate',
      rate: 100,
      timeUnit: '1s',
      duration: '1m',
      preAllocatedVUs: 100,
      maxVUs: 100,
    },
  },
  ext: {
    loadimpact: {
      distribution: {
        distributionLabel1: { loadZone: 'amazon:de:frankfurt', percent: 100 },
      },
    },
  },
};

const statusCodeCounter = new Counter('status_codes');
const jsonRpcErrorCounter = new Counter('jsonrpc_errors');
const parsingErrorsCounter = new Counter('parsing_errors');
const responseSizes = new Trend('response_sizes');

// Common ERC20 Transfer event topic
const TRANSFER_EVENT_TOPIC = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';

function getRandomChain() {
  const chains = Object.values(CHAINS);
  return chains[randomIntBetween(0, chains.length - 1)];
}

async function latestBlockWithLogs(http, params, chain) {
  const latestBlock = await getLatestBlock(http, params, chain);
  if (!latestBlock) return null;

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getLogs",
    params: [{
      fromBlock: latestBlock.number,
      toBlock: latestBlock.number,
      topics: [TRANSFER_EVENT_TOPIC]
    }],
    id: Math.floor(Math.random() * 100000000)
  });
  if (__ENV.DEBUG) {
    console.log(`Request: ${payload}`);
  }
  return http.post(ERPC_BASE_URL + chain.id, payload, params);
}

function randomAccountBalances(http, params, chain) {
  // Generate a random address-like string
  const randomAddr = '0x' + Array.from({length: 40}, () => 
    '0123456789abcdef'[randomIntBetween(0, 15)]).join('');

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBalance",
    params: [randomAddr, "latest"],
    id: Math.floor(Math.random() * 100000000)
  });
  if (__ENV.DEBUG) {
    console.log(`Request: ${payload}`);
  }
  return http.post(ERPC_BASE_URL + chain.id, payload, params);
}

async function getLatestBlock(http, params, chain) {
  const now = Date.now() / 1000;
  if (chain.cached.latestBlock && (now - chain.cached.latestBlockTimestamp) < CONFIG.LATEST_BLOCK_CACHE_TTL) {
    return chain.cached.latestBlock;
  }

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: ["latest", true], // Changed to true to get full transaction objects
    id: Math.floor(Math.random() * 100000000)
  });

  if (__ENV.DEBUG) {
    console.log(`Request: ${payload}`);
  }
  const res = await http.post(ERPC_BASE_URL + chain.id, payload, params);
  if (res.status === 200) {
    try {
      const body = JSON.parse(res.body);
      if (body.result) {
        chain.cached.latestBlock = body.result;
        chain.cached.latestBlockTimestamp = now;
        // Cache transaction hashes
        chain.cached.transactions = body.result.transactions
          .map(tx => typeof tx === 'string' ? tx : tx.hash)
          .slice(0, CONFIG.MAX_CACHED_TXS);
        return body.result;
      }
    } catch (e) {
      console.error(`Failed to parse latest block response: ${e}`);
    }
  }
  return null;
}

function getRandomCachedTransaction(chain) {
  if (!chain.cached.transactions || chain.cached.transactions.length === 0) {
    return null;
  }
  return chain.cached.transactions[randomIntBetween(0, chain.cached.transactions.length - 1)];
}

async function traceLatestTransaction(http, params, chain) {
  let txHash = getRandomCachedTransaction(chain);
  if (!txHash) {
    const latestBlock = await getLatestBlock(http, params, chain);
    if (!latestBlock) return null;
    txHash = getRandomCachedTransaction(chain);
  }

  if (!txHash) {
    return null;
  }
  
  // Standard Ethereum trace methods
  const traceMethods = [
    {
      method: "debug_traceTransaction",
      params: [txHash, { tracer: "callTracer" }]
    },
    {
      method: "trace_replayTransaction",
      params: [txHash, ["trace"]]
    },
    {
      method: "trace_transaction",
      params: [txHash]
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

    if (__ENV.DEBUG) {
      console.log(`Request: ${tracePayload}`);
    }
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
  return null;
}

async function latestBlockReceipts(http, params, chain) {
  let txHash = getRandomCachedTransaction(chain);
  if (!txHash) {
    const latestBlock = await getLatestBlock(http, params, chain);
    if (!latestBlock) return null;
    txHash = getRandomCachedTransaction(chain);
  }

  if (!txHash) {
    return null;
  }

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getTransactionReceipt",
    params: [txHash],
    id: Math.floor(Math.random() * 100000000)
  });
  if (__ENV.DEBUG) {
    console.log(`Request: ${payload}`);
  }
  return http.post(ERPC_BASE_URL + chain.id, payload, params);
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
      switch (pattern) {
        case 'LATEST_BLOCK_WITH_LOGS':
          res = await latestBlockWithLogs(http, params, selectedChain);
          break;
        case 'LATEST_BLOCK_RECEIPTS':
          res = await latestBlockReceipts(http, params, selectedChain);
          break;
        case 'LATEST_BLOCK_TRACES':
          res = await traceLatestTransaction(http, params, selectedChain);
          break;
        case 'RANDOM_ACCOUNT_BALANCES':
          res = randomAccountBalances(http, params, selectedChain);
          break;
      }
      break;
    }
  }
  
  // const sampleReq = {"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0xfaeff2","toBlock":"0xfaeff3","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}],"id":Math.ceil(Math.random() * 10000000)};
  // res = http.post(ERPC_BASE_URL, JSON.stringify(sampleReq), params);

  if (res) {
    tags['status_code'] = res.status.toString();
    statusCodeCounter.add(1, tags);
    responseSizes.add(res.body.length, tags);

    if (__ENV.DEBUG) {
      if (res.status >= 400) {
        console.log(`Status Code: ${res.status} Response body: ${res.body} Tags: ${JSON.stringify(tags)}`);
      }
    }

    let parsedBody = null;
    try {
      parsedBody = JSON.parse(res.body);
    } catch (e) {
      parsingErrorsCounter.add(1, tags);
      console.error(`Failed to parse response body: ${e}`);
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
  }
}

