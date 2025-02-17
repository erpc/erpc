import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

// Target URL (can be configured via environment variables)
// const TARGET_URL = __ENV.TARGET_URL || 'http://localhost:4000/main/evm/1';
const TARGET_URL = __ENV.TARGET_URL || 'https://c570.us-west.gcp.erpc.cloud/main/evm/1';

// Traffic pattern weights (in percentage, should sum to 100)
const TRAFFIC_PATTERNS = {
  RANDOM_HISTORICAL_BLOCKS: 15,      // Fetch random blocks from history
  LATEST_BLOCK_WITH_LOGS: 20,        // Get latest block and its transfer logs
  RANDOM_LOG_RANGES: 15,             // Get logs for random block ranges
  RANDOM_HISTORICAL_RECEIPTS: 15,    // Get random transaction receipts from history
  LATEST_BLOCK_RECEIPTS: 15,         // Get receipts from latest block's transactions
  RANDOM_ACCOUNT_BALANCES: 10,       // Get random account balances
  TRACE_RANDOM_TRANSACTIONS: 10,     // Trace random transactions with various methods
};

// Configuration
const CONFIG = {
  RANDOM_BLOCK_MIN: 0x0800000,       
  RANDOM_BLOCK_MAX: 0x1000000,       
  LOG_RANGE_MIN_BLOCKS: 1,
  LOG_RANGE_MAX_BLOCKS: 1,
  CACHED_LATEST_BLOCK: null,
  CACHED_LATEST_BLOCK_TIMESTAMP: 0,
  LATEST_BLOCK_CACHE_TTL: 5,         // seconds
};

// K6 configuration
export const options = {
  scenarios: {    
    constant_request_rate: {
      executor: 'constant-arrival-rate',
      rate: 5,
      timeUnit: '1s',
      duration: '10m',
      preAllocatedVUs: 500,
      maxVUs: 500,
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

const errorRate = new Rate('errors');

// Common ERC20 Transfer event topic
const TRANSFER_EVENT_TOPIC = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';

// Helper functions
function getRandomBlock() {
  return `0x${randomIntBetween(CONFIG.RANDOM_BLOCK_MIN, CONFIG.RANDOM_BLOCK_MAX).toString(16)}`;
}

function getRandomBlockRange() {
  const fromBlock = parseInt(getRandomBlock(), 16);
  const rangeSize = randomIntBetween(CONFIG.LOG_RANGE_MIN_BLOCKS, CONFIG.LOG_RANGE_MAX_BLOCKS);
  return {
    fromBlock: `0x${fromBlock.toString(16)}`,
    toBlock: `0x${(fromBlock + rangeSize).toString(16)}`
  };
}

async function getLatestBlock(http, params) {
  const now = Date.now() / 1000;
  if (CONFIG.CACHED_LATEST_BLOCK && (now - CONFIG.CACHED_LATEST_BLOCK_TIMESTAMP) < CONFIG.LATEST_BLOCK_CACHE_TTL) {
    return CONFIG.CACHED_LATEST_BLOCK;
  }

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: ["latest", false],
    id: Math.floor(Math.random() * 100000000)
  });

  const res = await http.post(TARGET_URL, payload, params);
  if (res.status === 200) {
    try {
      const body = JSON.parse(res.body);
      if (body.result) {
        CONFIG.CACHED_LATEST_BLOCK = body.result;
        CONFIG.CACHED_LATEST_BLOCK_TIMESTAMP = now;
        return body.result;
      }
    } catch (e) {
      console.error(`Failed to parse latest block response: ${e}`);
    }
  }
  return null;
}

// Traffic pattern implementations
function randomHistoricalBlocks(http, params) {
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(), false],
    id: Math.floor(Math.random() * 100000000)
  });
  return http.post(TARGET_URL, payload, params);
}

async function latestBlockWithLogs(http, params) {
  const latestBlock = await getLatestBlock(http, params);
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
  return http.post(TARGET_URL, payload, params);
}

function randomLogRanges(http, params) {
  const { fromBlock, toBlock } = getRandomBlockRange();
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
  return http.post(TARGET_URL, payload, params);
}

function randomHistoricalReceipts(http, params) {
  // First get a random block
  const blockPayload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(), true],
    id: Math.floor(Math.random() * 100000000)
  });
  
  const blockRes = http.post(TARGET_URL, blockPayload, params);
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
    return http.post(TARGET_URL, receiptPayload, params);
  } catch (e) {
    console.error(`Failed to process block response: ${e}`);
    return blockRes;
  }
}

async function latestBlockReceipts(http, params) {
  const latestBlock = await getLatestBlock(http, params);
  if (!latestBlock || !latestBlock.transactions || latestBlock.transactions.length === 0) return null;

  const randomTx = latestBlock.transactions[randomIntBetween(0, latestBlock.transactions.length - 1)];
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getTransactionReceipt",
    params: [randomTx],
    id: Math.floor(Math.random() * 100000000)
  });
  return http.post(TARGET_URL, payload, params);
}

function randomAccountBalances(http, params) {
  // Generate a random address-like string
  const randomAddr = '0x' + Array.from({length: 40}, () => 
    '0123456789abcdef'[randomIntBetween(0, 15)]).join('');

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBalance",
    params: [randomAddr, "latest"],
    id: Math.floor(Math.random() * 100000000)
  });
  return http.post(TARGET_URL, payload, params);
}

async function traceRandomTransaction(http, params) {
  // First get a random block with transactions
  const blockPayload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [getRandomBlock(), true],
    id: Math.floor(Math.random() * 100000000)
  });
  
  const blockRes = await http.post(TARGET_URL, blockPayload, params);
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

      const traceRes = await http.post(TARGET_URL, tracePayload, params);
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
  const rand = Math.random() * 100;
  let cumulativeWeight = 0;
  let res;

  // for (const [pattern, weight] of Object.entries(TRAFFIC_PATTERNS)) {
  //   cumulativeWeight += weight;
  //   if (rand <= cumulativeWeight) {
  //     switch (pattern) {
  //       case 'RANDOM_HISTORICAL_BLOCKS':
  //         res = randomHistoricalBlocks(http, params);
  //         break;
  //       case 'LATEST_BLOCK_WITH_LOGS':
  //         res = await latestBlockWithLogs(http, params);
  //         break;
  //       case 'RANDOM_LOG_RANGES':
  //         res = randomLogRanges(http, params);
  //         break;
  //       case 'RANDOM_HISTORICAL_RECEIPTS':
  //         res = randomHistoricalReceipts(http, params);
  //         break;
  //       case 'LATEST_BLOCK_RECEIPTS':
  //         res = await latestBlockReceipts(http, params);
  //         break;
  //       case 'RANDOM_ACCOUNT_BALANCES':
  //         res = randomAccountBalances(http, params);
  //         break;
  //       case 'TRACE_RANDOM_TRANSACTIONS':
  //         res = await traceRandomTransaction(http, params);
  //         break;
  //     }
  //     break;
  //   }
  // }
  const sampleReq = {"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0xfaeff2","toBlock":"0xfaeff3","topics":["0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"]}],"id":Math.ceil(Math.random() * 10000000)};
  res = http.post(TARGET_URL, JSON.stringify(sampleReq), params);

  if (res) {
    check(res, {
      'status is 200': (r) => r.status === 200,
      'response has no error': (r) => {
        const size = r?.body?.length;
        if (size > 1000000) {
          console.log(`Large response body: ${size} bytes found: ${r?.request?.body} ====> ${r?.body?.substring(0, 100)}`);
        }
        try {
          const body = JSON.parse(r.body);
          return body && (body.error === undefined || body.error === null);
        } catch (e) {
          if (size > 200) {
            const head = r.body.substring(0, 5000);
            const tail = r.body.substring(size - 5000);
            console.log(`Unmarshal error: "${e}" for ${size} bytes body: REQUEST=${r?.request?.body} ===> RESPONSE=${head}...${tail}`);
          } else {
            console.log(`Unmarshal error: "${e}" for ${size} bytes body: REQUEST=${r?.request?.body} ===> RESPONSE=${r.body}`);
          }
          return false;
        }
      },
    });

    errorRate.add(res.status !== 200);
  }

  sleep(1);
}
