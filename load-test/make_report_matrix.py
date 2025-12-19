#!/usr/bin/env python3
import json
import os
import sys
import matplotlib.pyplot as plt

RPS_LEVELS = [100, 250, 400, 550, 700, 850, 1000]

def load_json(path):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)

def metric(d, name):
    return d.get("metrics", {}).get(name, {})

def val(m, key, default=None):
    return m.get(key, default)

def main(out_dir: str):
    xs = []
    p50, p95, p99 = [], [], []
    throughput = []
    err_rate = []

    for rps in RPS_LEVELS:
        path = os.path.join(out_dir, f"summary_rps_{rps}.json")
        if not os.path.exists(path):
            print("Missing:", path)
            sys.exit(1)

        d = load_json(path)

        dur = metric(d, "http_req_duration")
        p50v = val(dur, "med")              # k6 uses med for p50
        p95v = val(dur, "p(95)")
        p99v = val(dur, "p(99)", p95v)      # fallback if p99 missing

        reqs = metric(d, "http_reqs")
        achieved_rps = val(reqs, "rate", 0)

        failed = metric(d, "http_req_failed")
        err = val(failed, "value", 0)

        xs.append(rps)
        p50.append(p50v)
        p95.append(p95v)
        p99.append(p99v)
        throughput.append(achieved_rps * (1 - err))
        err_rate.append(err)

    # === Latency graph ===
    plt.figure()
    plt.plot(xs, p50, marker="o", label="p50")
    plt.plot(xs, p95, marker="o", label="p95")
    plt.plot(xs, p99, marker="o", label="p99")
    plt.xlabel("Target RPS")
    plt.ylabel("Latency (ms)")
    plt.title("Redirect latency vs RPS")
    plt.legend()
    plt.savefig(os.path.join(out_dir, "latency_vs_rps.png"), dpi=160)
    plt.close()

    # === Throughput graph ===
    plt.figure()
    plt.plot(xs, throughput, marker="o")
    plt.xlabel("Target RPS")
    plt.ylabel("Successful requests / second")
    plt.title("Throughput vs RPS")
    plt.savefig(os.path.join(out_dir, "throughput_vs_rps.png"), dpi=160)
    plt.close()

    # === Error rate graph ===
    plt.figure()
    plt.plot(xs, err_rate, marker="o")
    plt.xlabel("Target RPS")
    plt.ylabel("Error rate")
    plt.title("Error rate vs RPS")
    plt.savefig(os.path.join(out_dir, "errors_vs_rps.png"), dpi=160)
    plt.close()

    print("Generated graphs in:", out_dir)

if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: python3 load-test/make_report_matrix.py load-test/out/<timestamp>")
        sys.exit(1)

    main(sys.argv[1])
