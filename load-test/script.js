import http from "k6/http";
import { check } from "k6";
import { Trend, Rate } from "k6/metrics";

const latency = new Trend("redirect_latency_ms", true);
const okRate = new Rate("ok_rate");
const errRate = new Rate("err_rate");

const BASE = __ENV.BASE || "http://localhost:8080";
const CODE = __ENV.CODE || "";
const TARGET_PATH = __ENV.TARGET_PATH || `/r/${CODE}`;
const TARGET_RPS = Number(__ENV.TARGET_RPS || "100");

export const options = {
  // 👇 THIS is what adds p99
  summaryTrendStats: ["avg", "min", "med", "p(50)", "p(95)", "p(99)", "max"],

  scenarios: {
    single: {
      executor: "constant-arrival-rate",
      rate: TARGET_RPS,
      timeUnit: "1s",
      duration: "20s",
      preAllocatedVUs: 2000,
      maxVUs: 20000,
      exec: "hit",
    },
  },

  thresholds: {
    http_req_failed: ["rate<0.02"],
  },
};

export function hit() {
  if (!CODE) throw new Error("Missing CODE env var");

  const res = http.get(`${BASE}${TARGET_PATH}`, { redirects: 0 });
  const ok = res.status === 302;

  if (!ok && Math.random() < 0.001) {
    console.log(
      "FAIL:",
      "status=", res.status,
      "error=", res.error,
      "dur_ms=", res.timings.duration
    );
  }

  check(res, { "status is 302": () => ok });

  latency.add(res.timings.duration);
  okRate.add(ok);
  errRate.add(!ok);
}
