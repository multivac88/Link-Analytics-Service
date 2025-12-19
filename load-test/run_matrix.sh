#!/usr/bin/env bash
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
: "${CODE:?Missing CODE env var (your short code)}"

TS="$(date +%Y%m%d_%H%M%S)"
OUTDIR="load-test/out/${TS}"
mkdir -p "${OUTDIR}"

# quick preflight
STATUS="$(curl -s -o /dev/null -w "%{http_code}" -I "${BASE}/r/${CODE}" || true)"
if [[ "${STATUS}" != "302" ]]; then
  echo "ERROR: ${BASE}/r/${CODE} returned HTTP ${STATUS} (expected 302)."
  exit 1
fi

echo "Running 7-stage matrix test. Output: ${OUTDIR}"

for r in 100 250 400 550 700 850 1000; do
  echo "== Running TARGET_RPS=${r} =="
  TARGET_RPS="${r}" k6 run \
    -e BASE="${BASE}" \
    -e CODE="${CODE}" \
    --summary-export="${OUTDIR}/summary_rps_${r}.json" \
    load-test/script.js
done

echo "Done. Summaries in ${OUTDIR}"
echo "Now generate graphs:"
echo "python3 load-test/make_report_matrix.py ${OUTDIR}"
