import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export const options = {
  stages: [
    { duration: '10s', target: 800 },
    { duration: '30s', target: 800 },
  ],
};

const samplePayload = JSON.stringify({
  "jsonrpc": "2.0",
  "method": "eth_getBlockByNumber",
  "params": [
    "0x1346edf",
    false
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
