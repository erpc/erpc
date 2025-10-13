import http from 'k6/http';
import { check, randomSeed } from 'k6';
import { Rate, Counter, Trend, Gauge } from 'k6/metrics';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.4/index.js';

// Target URL (can be configured via environment variables)
const ERPC_BASE_URL = __ENV.ERPC_BASE_URL || 'http://localhost:4000/main/evm/';

// Traffic pattern weights (in percentage, should sum to 100)
const TRAFFIC_PATTERNS = {
  RECENT_BLOCK_FEW_BLOCKS: 30,        // Get blockByNumber for a few blocks back
  LATEST_BLOCK_WITH_LOGS: 10,        // Get latest block and its transfer logs
  LATEST_BLOCK_RECEIPTS: 30,         // Get receipts from latest block's transactions
  LATEST_BLOCK_TRACES: 20,           // Get traces from latest block's transactions
  RANDOM_ACCOUNT_BALANCES: 10,       // Get random account balances
};

// Configuration
const CONFIG = {   
  LOG_RANGE_MIN_BLOCKS: 1,
  LOG_RANGE_MAX_BLOCKS: 100,
  LATEST_BLOCK_CACHE_TTL: 1,         // reduced to 1 second
  MAX_CACHED_TXS: 50,                // maximum number of transaction hashes to cache
};

const API_KEYS = typeof __ENV.API_KEYS === 'string'
  ? __ENV.API_KEYS
      .split(',')
      .map((key) => key.trim())
      .filter((key) => key.length > 0)
  : [];

// Update CHAINS structure to include transaction cache
const CHAINS = {
  // ETH: {
  //   id: '1',
  //   cached: {
  //     latestBlock: null,
  //     latestBlockTimestamp: 0,
  //     transactions: []
  //   }
  // },
  POLYGON: {
    id: '137',
    cached: {
      latestBlock: null,
      latestBlockTimestamp: 0,
      transactions: []
    }
  },
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
      rate: 500,
      timeUnit: '1s',
      duration: '20s',
      preAllocatedVUs: 100,
      maxVUs: 1000,
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
const truncatedResponseCounter = new Counter('truncated_responses');
const retriesCounter = new Counter('request_retries');
const responseSizes = new Trend('response_sizes');
const STATUS_CODE_METRIC_PREFIX = 'status_code_';
const STATUS_CODE_COUNTERS = (() => {
  const counters = new Map();
  counters.set('unknown', new Counter(`${STATUS_CODE_METRIC_PREFIX}unknown`));
  for (let code = 100; code <= 599; code += 1) {
    const key = code.toString();
    counters.set(key, new Counter(`${STATUS_CODE_METRIC_PREFIX}${key}`));
  }
  return counters;
})();
const statusCodeCounters = (() => {
  const counters = new Map();
  counters.set('unknown', new Counter('status_code_unknown'));
  for (let code = 100; code <= 599; code += 1) {
    const codeKey = code.toString();
    counters.set(codeKey, new Counter(`status_code_${codeKey}`));
  }
  return counters;
})();

// Common ERC20 Transfer event topic
const TRANSFER_EVENT_TOPIC = '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';

function getFullUrl(chain) {
  // K6 does not have the global URL class, so we must parse manually.
  // We want to insert the chain.id after the path, before any query string.
  // Example: "https://foo.com/api?x=1" + "42161" => "https://foo.com/api/42161?x=1"
  let base = ERPC_BASE_URL;
  let query = '';
  let path = base;
  const qIdx = base.indexOf('?');
  if (qIdx !== -1) {
    path = base.substring(0, qIdx);
    query = base.substring(qIdx); // includes '?'
  }
  // Ensure path ends with a single slash before appending chain.id
  if (!path.endsWith('/')) path += '/';
  const finalPath = path + chain.id;
  const apiKey = getRandomApiKey();
  const existingQuery = query.startsWith('?') ? query.slice(1) : query;
  const querySegments = existingQuery ? [existingQuery] : [];
  if (__ENV.TRACE) {
    console.log(`${new Date().toISOString()} API Key: ${apiKey}`);
  }
  if (apiKey) {
    querySegments.unshift(`secret=${encodeURIComponent(apiKey)}`);
  }
  if (querySegments.length === 0) {
    return finalPath;
  }
  return `${finalPath}?${querySegments.join('&')}`;
}

function getRandomChain() {
  const chains = Object.values(CHAINS);
  return chains[randomIntBetween(0, chains.length - 1)];
}

function getRandomApiKey() {
  if (API_KEYS.length === 0) {
    return null;
  }
  return API_KEYS[randomIntBetween(0, API_KEYS.length - 1)];
}

function getStatusCodeTotals(metrics) {
  const totals = new Map();
  if (!metrics || typeof metrics !== 'object') {
    return totals;
  }
  for (const [metricName, metric] of Object.entries(metrics)) {
    if (!metricName.startsWith(STATUS_CODE_METRIC_PREFIX)) {
      continue;
    }
    const status = metricName.substring(STATUS_CODE_METRIC_PREFIX.length);
    const count = metric && metric.values && typeof metric.values.count === 'number'
      ? metric.values.count
      : 0;
    if (count === 0) {
      continue;
    }
    totals.set(status, (totals.get(status) || 0) + count);
  }
  if (totals.size > 0) {
    return totals;
  }
  const statusMetric = metrics.status_codes;
  if (!statusMetric) {
    return totals;
  }
  const candidateCollections = [];
  if (statusMetric.tags && typeof statusMetric.tags === 'object') {
    candidateCollections.push(statusMetric.tags);
  }
  if (statusMetric.submetrics && typeof statusMetric.submetrics === 'object') {
    candidateCollections.push(statusMetric.submetrics);
  }
  if (statusMetric.valuesByTag && typeof statusMetric.valuesByTag === 'object') {
    candidateCollections.push(statusMetric.valuesByTag);
  }
  const extractCount = (entry) => {
    if (entry == null) {
      return 0;
    }
    if (typeof entry === 'number') {
      return entry;
    }
    if (typeof entry === 'object') {
      if (typeof entry.count === 'number') {
        return entry.count;
      }
      if (entry.values && typeof entry.values === 'object') {
        if (typeof entry.values.count === 'number') {
          return entry.values.count;
        }
        if (typeof entry.values.value === 'number') {
          return entry.values.value;
        }
      }
      if (typeof entry.value === 'number') {
        return entry.value;
      }
    }
    return 0;
  };
  for (const collection of candidateCollections) {
    for (const [tagKey, metric] of Object.entries(collection)) {
      const match = /status_code:([^,]+)/.exec(tagKey);
      if (!match) {
        continue;
      }
      const count = extractCount(metric);
      if (count === 0) {
        continue;
      }
      const status = match[1];
      totals.set(status, (totals.get(status) || 0) + count);
    }
  }
  return totals;
}

async function latestBlockWithLogs(http, params, chain) {
  const latestBlock = await getLatestBlock(http, params, chain);
  if (!latestBlock) return null;

  const decimalBlockNumber = parseInt(latestBlock.number, 16);
  const randomShift = randomIntBetween(0, 5000);
  const randomToLimit = randomIntBetween(0, randomShift);

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getLogs",
    params: [{
      fromBlock: '0x' + Math.max(0, decimalBlockNumber - randomShift).toString(16),
      toBlock: '0x' + Math.max(0, decimalBlockNumber - randomShift + randomToLimit).toString(16),
      topics: [TRANSFER_EVENT_TOPIC]
    }],
    id: Math.floor(Math.random() * 100000000)
  });
  if (__ENV.TRACE) {
    console.log(`Request: ${payload}`);
  }
  return makeRequestWithRetry(http, getFullUrl(chain), payload, params);
}

async function recentBlockFewBlocks(http, params, chain) {
  const latestBlock = await getLatestBlock(http, params, chain);
  if (!latestBlock) {
    if (__ENV.DEBUG || __ENV.TRACE) {
      console.warn(`${new Date().toISOString()} No latest block for ${chain.id}`);
    }
    return null;
  } else {
    if (__ENV.DEBUG || __ENV.TRACE) {
      console.log(`${new Date().toISOString()} Latest block for ${chain.id}: ${latestBlock?.number}`);
    }
  }

  const randomShift = randomIntBetween(0, 500);
  const decimalBlockNumber = parseInt(latestBlock.number, 16);

  const payload = JSON.stringify({
    jsonrpc: "2.0",
    method: "eth_getBlockByNumber",
    params: [
      '0x' + Math.max(0, decimalBlockNumber - randomShift).toString(16),
      true
    ],
    id: Math.floor(Math.random() * 100000000)
  });
  if (__ENV.TRACE) {
    console.log(`Request: ${payload}`);
  }
  return makeRequestWithRetry(http, getFullUrl(chain), payload, params);
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
  if (__ENV.TRACE) {
    console.log(`Request: ${payload}`);
  }
  return makeRequestWithRetry(http, getFullUrl(chain), payload, params);
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

  if (__ENV.TRACE) {
    console.log(`Request: ${payload}`);
  }
  const res = await makeRequestWithRetry(http, getFullUrl(chain), payload, params);
  if (res.status === 200) {
    if (__ENV.TRACE) {
      console.log(`${new Date().toISOString()} Latest block body received for ${chain.id}: ${res?.body?.length}`);
    }
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
  } else {
    if (__ENV.DEBUG || __ENV.TRACE) {
      console.warn(`${new Date().toISOString()} Could not get latest block for ${chain.id}: ${res?.body || JSON.stringify(res)}`);
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

    if (__ENV.TRACE) {
      console.log(`Request: ${tracePayload}`);
    }
    const traceRes = await makeRequestWithRetry(http, getFullUrl(chain), tracePayload, params);
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
  if (__ENV.TRACE) {
    console.log(`Request: ${payload}`);
  }
  return makeRequestWithRetry(http, getFullUrl(chain), payload, params);
}

function randomIntBetween(min, max) {
  return Math.floor(Math.random() * (max - min + 1)) + min;
}

function truncateResponseBody(body) {
  if (!body || body.length <= 128) {
    return body;
  }
  const first64 = body.substring(0, 64);
  const last64 = body.substring(body.length - 64);
  return `${first64}...${last64}`;
}

function isValidJsonResponse(body) {
  if (!body || body.length === 0) return false;
  
  // Check if response looks truncated (common patterns)
  if (body.endsWith('...') || !body.endsWith('}') && !body.endsWith(']')) {
    return false;
  }
  
  try {
    const parsed = JSON.parse(body);
    return parsed && (parsed.result !== undefined || parsed.error !== undefined);
  } catch (e) {
    return false;
  }
}

async function makeRequestWithRetry(http, url, payload, params, maxRetries = 2) {
  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    const res = await http.post(url, payload, params);
    
    if (res && res.status === 200 && isValidJsonResponse(res.body)) {
      if (attempt > 0) {
        retriesCounter.add(attempt);
      }
      return res;
    }
    
    // Log truncated responses for debugging
    if (res && res.status === 200 && !isValidJsonResponse(res.body)) {
      truncatedResponseCounter.add(1);
      if (__ENV.DEBUG || __ENV.TRACE) {
        console.warn(`${new Date().toISOString()} Truncated response detected (attempt ${attempt + 1}): ${truncateResponseBody(res.body)}`);
      }
    }
    
    // Don't retry on the last attempt
    if (attempt < maxRetries) {
      // Small delay before retry to avoid overwhelming the server
      await new Promise(resolve => setTimeout(resolve, 100 + Math.random() * 200));
    } else {
      if (attempt > 0) {
        retriesCounter.add(attempt);
      }
      return res; // Return the last response even if invalid
    }
  }
}

// Main test function
export default async function () {
  const params = {
    headers: { 
      'Content-Type': 'application/json',
      'Accept-Encoding': 'gzip, deflate',
      'Connection': 'keep-alive'
    },
    insecureSkipTLSVerify: true,
    timeout: '60s',  // Increased timeout
    // Force HTTP/1.1 to avoid HTTP/2 multiplexing issues under load
    httpVersion: '2.0',
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
      if (__ENV.TRACE) {
        console.log(`${new Date().toISOString()} Pattern: ${pattern} Weight: ${weight} Rand: ${rand} Cumulative Weight: ${cumulativeWeight}`);
      }
      switch (pattern) {
        case 'RECENT_BLOCK_FEW_BLOCKS':
          res = await recentBlockFewBlocks(http, params, selectedChain);
          break;
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
    const status = typeof res.status === 'number' ? res.status.toString() : 'unknown';
    if (status !== 'unknown') {
      tags.status_code = status;
    }
    statusCodeCounter.add(1, tags);
    const statusCounter = STATUS_CODE_COUNTERS.get(status) || STATUS_CODE_COUNTERS.get('unknown');
    statusCounter.add(1, { chain: selectedChain.id });
    if (res.body) {
      responseSizes.add(res.body?.length, tags);
    }

    if (__ENV.DEBUG || __ENV.TRACE) {
      if (!res?.status) {
        console.warn(`${new Date().toISOString()} No status code for ${selectedChain.id}: ${JSON.stringify(res?.body || res)}`);
      } else if (res.status > 210) {
        console.warn(`${new Date().toISOString()} Status Code: ${res?.status || 'n/a'} Response body: ${truncateResponseBody(res?.body || 'n/a')} Tags: ${JSON.stringify(tags)}`);
      }
    }

    let parsedBody = null;
    try {
      parsedBody = JSON.parse(res.body);
    } catch (e) {
      parsingErrorsCounter.add(1, tags);
      // console.error(`${new Date().toISOString()} Failed to parse response body: ${e} response body: ${truncateResponseBody(res.body)}`);
      if (res.body) {
        tags['generic_error_code'] = 'PARSING_ERROR';
        tags['generic_error_message'] = res.body;
      } else {
        tags['generic_error_code'] = 'PARSING_ERROR';
        tags['generic_error_message'] = 'No response body';
      }
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
    if (__ENV.DEBUG || __ENV.TRACE) {
      console.warn(`${new Date().toISOString()} No response for ${selectedChain.id}`);
    }
  }
}

export function handleSummary(data) {
  const metrics = (data && data.metrics) || {};
  const statusTotals = getStatusCodeTotals(metrics);
  const reportLines = [];
  if (statusTotals.size > 0) {
    reportLines.push('Status code totals:');
    const sorted = Array.from(statusTotals.entries()).sort((a, b) => Number(a[0]) - Number(b[0]));
    for (const [status, count] of sorted) {
      reportLines.push(`  ${status}: ${count}`);
    }
  } else if (metrics.status_codes && metrics.status_codes.values && typeof metrics.status_codes.values.count === 'number') {
    reportLines.push(`Total status codes recorded: ${metrics.status_codes.values.count}`);
  }
  const statusReport = reportLines.length > 0 ? `\n${reportLines.join('\n')}\n` : '\n';
  return {
    stdout: `${textSummary(data, { indent: ' ', enableColors: true })}${statusReport}`,
  };
}

