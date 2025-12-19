
Why stepped rps ranges
- to prevent test tool from becoming the bottleneck
at higher rps the disk i/o and logging was creating artificial latency. First tests only reached up to 500 RPS.
After compressing JSON output and seperating rps stages did I reach 1000.

Conclusion:
At around 300 RPS, p99 latency begins to increase sharply while p50 and p95 remain stable.
This is expected behavior caused by queueing and contention in a fully synchronous system.

During a request, a redirect performs a database read and a synchronous click insert on every request (one block of work per request).
As requests increase, a small fraction of requests experience db connection pool waits, which are reflected in p99.

For connection pool waits, my go app uses pgxpool which provides connections pools of around 4-10 connections to the db (connections are expensive). 
A requests borrows one connection and returns it after use. If their are no connections available, this creates latency. 
But this latency happens periodically that is why p50, p95 are stable but p99 at >300 RPS increases dramatically.

Importantly, throughput continues to scale and error rate remains zero, indicating the system is not saturated but approaching its tail-latency limits.

In a production system, this would be mitigated by async click logging, batching, or a write-behind queue (e.g. Kafka/Redis), which were intentionally omitted for MVP clarity.

THree graphs 
errors_vs_rps.png
latency_vs_rps.png
throughput_vs_rps.png are in the load_test folder
