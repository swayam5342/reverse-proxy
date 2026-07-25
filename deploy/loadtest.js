// k6 load test baseline for the gateway. Run: k6 run deploy/loadtest.js
// Point BASE_URL at a running gateway with a route that needs no auth, or
// export API_KEY to exercise an authenticated route.
//
// Baseline recorded on <fill in your dev machine spec>, single gateway
// instance, single upstream echo server, GET /api/<route>:
//   RPS: <fill in>   p50: <fill in>ms   p99: <fill in>ms
// Re-run and update these numbers whenever proxy/middleware perf-sensitive
// code changes, so a regression shows up as a diff in this file.
import http from "k6/http";
import { check, sleep } from "k6";

export const options = {
  vus: 50,
  duration: "30s",
  thresholds: {
    http_req_duration: ["p(50)<50", "p(99)<250"],
    http_req_failed: ["rate<0.01"],
  },
};

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const API_KEY = __ENV.API_KEY || "";

export default function () {
  const res = http.get(`${BASE_URL}/api/users/ping`, {
    headers: API_KEY ? { "X-API-Key": API_KEY } : {},
  });
  check(res, { "status is 200 or 401": (r) => r.status === 200 || r.status === 401 });
  sleep(0.1);
}
