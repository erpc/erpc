import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
  stages: [
    { duration: '20s', target: 100 },
    { duration: '5m', target: 100 },
  ],
  ext: {
    loadimpact: {
      distribution: {
        distributionLabel1: { loadZone: 'amazon:de:frankfurt', percent: 80 },
        distributionLabel2: { loadZone: 'amazon:gb:london', percent: 20 },
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
const samplePayload = JSON.stringify({
  "jsonrpc": "2.0",
  "method": "debug_traceTransaction",
  "params": [
    "0xe6c2decd68012e0245599ddf93c232bf92884758393a502852cbf2f393e3d99c"
  ]
});

export default function () {
  const params = {
    headers: { 'Content-Type': 'application/json' },
  };

  // const res = http.post('http://localhost:8081', samplePayload, params);
  const res = http.post('http://localhost:4000/main/evm/123', samplePayload, params);

  check(res, {
    'status is 200': (r) => r.status === 200,
    'response has no error': (r) => {
      const body = JSON.parse(r.body);
      return body && (body.error === undefined || body.error === null);
    },
  });

  errorRate.add(res.status !== 200);

  sleep(1);
}
