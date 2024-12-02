import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
  scenarios: {    
    constant_request_rate: {
      executor: 'constant-arrival-rate',
      rate: 2000,
      timeUnit: '1s',
      duration: '10m',
      preAllocatedVUs: 10,
      maxVUs: 2000,
    },
  },
  ext: {
    loadimpact: {
      distribution: {
        distributionLabel1: { loadZone: 'amazon:de:frankfurt', percent: 100 },
        // distributionLabel2: { loadZone: 'amazon:gb:london', percent: 20 },
      },
    },
  },
};

// const samplePayload = JSON.stringify({
//   "jsonrpc": "2.0",
//   "method": "eth_getBlockByNumber",
//   "params": [
//     "0x1346edf",
//     false
//   ]
// });
// const samplePayload = JSON.stringify({
//   "jsonrpc": "2.0",
//   "method": "debug_traceTransaction",
//   "params": [
//     "0xe6c2decd68012e0245599ddf93c232bf92884758393a502852cbf2f393e3d99c"
//   ]
// });

export default function () {
  const params = {
    headers: { 'Content-Type': 'application/json' },
    insecureSkipTLSVerify: true,
  };

  const samplePayload = JSON.stringify({
    "jsonrpc": "2.0",
    "method": "eth_getBlockByNumber",
    "params": [
      `0x1${Math.floor(Math.random() * 0xFFFFF).toString(16).padStart(5, '0')}`,
      false
    ]
  });

  // const res = http.post('http://localhost:8081', samplePayload, params);
  // const res = http.post('http://142.132.247.137/aram-test/evm/42161', samplePayload, params);
  // const res = http.post('https://flair-test-default.hosted.erpc.cloud/aram-test/evm/42161', samplePayload, params);
  const res = http.post('https://a0c8339.eu-central.dch.erpc.cloud/aram-test/evm/42161', samplePayload, params);
  // const res = http.post('http://162.55.155.59/aram-test/evm/42161', samplePayload, params);

  check(res, {
    'status is 200': (r) => r.status === 200,
    'response has no error': (r) => {
      try {
        const body = JSON.parse(r.body);
        return body && (body.error === undefined || body.error === null);
      } catch (e) {
        console.log(`Unmarshal error: ${e} for body: ${r.body}`);
        return false;
      }
    },
  });

  errorRate.add(res.status !== 200);

  sleep(1);
}
