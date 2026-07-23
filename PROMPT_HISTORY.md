# Prompt History (index)

Tidy index into the raw log in **[TRANSCRIPT.md](TRANSCRIPT.md)**. Each row links
to the corresponding raw entry. Session date: 2026-07-23.

| # | Prompt (short) | Outcome | Raw |
|---|----------------|---------|-----|
| 1 | Fastest architecture for 1M personalized emails? | Recommended worker pool + rate limiter + ESP; SES + SQS pick | [T1](TRANSCRIPT.md) |
| 2 | Can we use Go goroutines/concurrency? | Confirmed; ESP quota is the ceiling, not Go | [T2](TRANSCRIPT.md) |
| 3 | Build the full Go program | Full pipeline: streaming source, pool, retry, checkpoint, DLQ | [T3](TRANSCRIPT.md) |
| 4 | README | Architecture + usage docs | [T4](TRANSCRIPT.md) |
| 5 | VPS? | SMTP backend + deliverability guide | [T5](TRANSCRIPT.md) |
| 6 | Dry-run | `-dryrun` wraps any backend | [T6](TRANSCRIPT.md) |
| 7 | Pool/buffered channel added? | Confirmed with file:line | [T7](TRANSCRIPT.md) |
| 8 | git + docker + .env | Repo, Dockerfile, compose, `.env` loader | [T8](TRANSCRIPT.md) |
| 9 | Logging? | Structured `slog`, text/json | [T9](TRANSCRIPT.md) |
| 10 | Senior review | Fixed 4 issues | [T10](TRANSCRIPT.md) |
| 11 | Fix top limit | SMTP connection pooling | [T11](TRANSCRIPT.md) |
| 12 | Update docs | README + `.env.example` | [T12](TRANSCRIPT.md) |
| 13–14 | Commit + push | Created + pushed repo | [T13](TRANSCRIPT.md) |
| 15 | Prompt history to md | This file | [T15](TRANSCRIPT.md) |
| 16–18 | Public / private toggles | Visibility changes | [T16](TRANSCRIPT.md) |
| 19 | 3-block work list (ID) | html/template, List-Unsubscribe, dedup, gencsv, 1M benchmark, crash+resume | [T19](TRANSCRIPT.md) |
| 20 | 5 tests + CI + repo meta | Unit tests, GitHub Actions, badge, description/topics | [T20](TRANSCRIPT.md) |
| 21 | Final English summary | Decision + benchmark + assumptions + not-done | [T21](TRANSCRIPT.md) |

## Architecture decisions captured

- **ESP quota, not Go, is the throughput ceiling.** Fixed worker count ≈
  `quota × latency`; more goroutines just queue.
- **Streaming everywhere** — 1M rows never held in RAM (measured peak RSS ~270MB).
- **At-least-once delivery.** Idempotency key + resumable checkpoint; true
  exactly-once needs provider-side dedup (out of scope).
- **Pluggable `Sender`** — mock / SMTP (pooled) / SES, swappable without touching
  pipeline code. SES behind a build tag to keep the default build dependency-free.
- **Single node by design.** Multi-node durability = swap the in-process channel
  for SQS/Kafka; pipeline logic ports unchanged.

See [TRANSCRIPT.md](TRANSCRIPT.md) for the raw log and [README.md](README.md) for usage.
