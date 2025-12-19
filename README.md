# Load Test Report — Redirect Endpoint

## Tool
k6 (`ramping-arrival-rate`)

## Scenario
- Endpoint: `GET /r/{code}` (redirects disabled in k6)
- Ramp: 100 → 1000 RPS (stepped)
- Duration: 140 seconds (~2m20s)

## How to Run
0. install k6 tool
1. Start stack:

docker compose up --build

makes:
    PostgreSQL

    Go backend on http://localhost:8080

    Next.js frontend on http://localhost:3000

2. Create a short link and copy its code in frontend copy ...r/[YOUR_CODE]

3. Run test from project root dir:
- chmod +x ./load-test/run_matrix.sh
- CODE=YOUR_CODE ./load-test/run_matrix.sh
- results will be saved to load-test/out/...

4. create python environment for generating plots:

python3 -m venv .venv
source .venv/bin/activate
pip install matplotlib

5. generate graphs:
python3 load-test/make_report_matrix.py load-test/out/<timestamp>


# Link-Analytics-Service
