# Prompt History

Chronological record of the prompts that drove this project, and what each
produced. Session date: 2026-07-23.

| # | Prompt | Outcome |
|---|--------|---------|
| 1 | *Design the fastest architecture for sending 1 million personalized emails — which is good for me?* | Recommended fan-out queue + worker pool + ESP bulk API. Compared SES / SendGrid / Kafka. Picked **SES + SQS + worker pool** (cheapest, scalable). Listed deliverability must-haves. |
| 2 | *Can we go because we can use goroutines and concurrency?* | Confirmed Go fits (I/O-bound, cheap goroutines). Sketched worker pool + `rate.Limiter` + `errgroup`. Established ESP quota is the real ceiling, not Go. |
| 3 | *Build the full Go program.* | Built the full project: streaming CSV source, parse-once templates, worker pool, rate limiter, retry+backoff, checkpoint, DLQ, mock/SES backends. Verified builds + live runs. |
| 4 | *README please.* | Added `README.md` with architecture diagram, components, usage. |
| 5 | *If the other side uses a VPS, how?* | Added **SMTP backend** (stdlib, no deps) for self-hosted VPS mail servers / relays. Documented SPF/DKIM/DMARC/PTR/IP-warmup reality. |
| 6 | *Don't forget dry-run code.* | Added `-dryrun` — wraps any backend, renders + counts, sends nothing. Safe pre-blast validation. |
| 7 | *Worker pool and buffered channel already added?* | Confirmed with `blast.go` line refs; explained backpressure + fixed-N design. |
| 8 | *Create git, also docker and .env.* | Added `.env` loader + `.env.example`, `.gitignore`, `.dockerignore`, `Dockerfile` (multi-stage, SES via build arg), `docker-compose.yml`. Env-backed flag defaults. |
| 9 | *How logging?* | Migrated to structured `log/slog` with `-log-format text\|json` + `-log-level`. Bridged stdlib `log` through the same handler. |
| 10 | *Review as senior backend.* | Self-review. Fixed: checkpoint durability (periodic flush), reinvented `errors.Is` → stdlib, CSV double-copy → `strings.Clone`, `-env=x` parsing. Documented remaining limits. |
| 11 | *Do issue first.* → *fix top remaining limit* | Fixed the top throughput limit: **SMTP connection pooling** — persistent pooled connections + `RSET` reuse, amortizing the TCP+TLS+AUTH handshake. Tunable `-smtp-pool`. |
| 12 | *Update all docs.* | Updated `README.md` (pooling, logging section, flags) + `.env.example` (`SMTP_POOL`). |
| 13–15 | *Commit, then push. Write prompt history to md.* | Committed each stage; created remote + pushed; wrote this file. |

## Architecture decisions captured

- **ESP quota, not Go, is the throughput ceiling.** Fixed worker count ≈
  `quota × latency`; more goroutines just queue.
- **Streaming everywhere** — 1M rows never held in RAM.
- **At-least-once delivery.** Idempotency key + resumable checkpoint; true
  exactly-once needs provider-side dedup (out of scope).
- **Pluggable `Sender`** — mock / SMTP (pooled) / SES, swappable without touching
  pipeline code. SES behind a build tag to keep the default build dependency-free.
- **Single node by design.** Multi-node durability = swap the in-process channel
  for SQS/Kafka; pipeline logic ports unchanged.

See `README.md` for full usage.
