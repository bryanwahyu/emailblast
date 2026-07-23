# emailblast

Fast, resumable, personalized bulk email sender in Go. Built for **1M+ recipients**
on a single node using a bounded goroutine worker pool.

The real ceiling is your **ESP sending quota**, not Go. Goroutines are cheap;
the pool just hides network latency behind a shared rate limiter that respects
that quota.

## Architecture

```
 CSV (streamed, never fully in RAM)
        │
   producer goroutine ── skips IDs already in checkpoint
        │
   jobs channel (buffered  →  backpressure, bounded memory)
        │
   ┌────┴────┬──────────┬──────────┐
 worker    worker     worker   … N goroutines
   │          │          │
   └── rate.Limiter (shared, = ESP quota) ──┘
        │
   render template  →  Sender.Send()  →  ESP
        │                                  │
   success → checkpoint.Done(id)      fail → retry w/ exp backoff
                                            └ exhausted → dead-letter.log
```

**Why fixed workers, not one goroutine per user:** 1M goroutines would flood the
ESP and waste memory. N workers where `N ≈ quota × latency` is enough to saturate
the quota. Everything above that just queues.

## Components

| File | Role |
|------|------|
| `internal/source/csv.go` | Streams recipients lazily (1M rows never in RAM) |
| `internal/render/render.go` | Parse-once `text/template`, concurrent-safe merge |
| `internal/blast/blast.go` | Worker pool, rate limiter, retry/backoff, DLQ |
| `internal/checkpoint/checkpoint.go` | Resumable progress log (crash-safe) |
| `internal/sender/sender.go` | `Sender` interface + `mock` backend (default) |
| `internal/sender/smtp.go` | SMTP backend (**pooled** persistent conns) — your **VPS** / relay |
| `internal/sender/ses.go` | Amazon SES v2 backend (build tag `ses`) |
| `internal/sender/dryrun.go` | Wraps any backend — renders + counts, sends nothing |

## Quick start (zero external services)

```bash
go run . -in users.csv -backend mock -workers 200 -rate 500 -verbose
```

Input CSV needs `id` and `email` columns; every other column is a merge field:

```csv
id,email,first_name,plan
u001,alice@example.com,Alice,Pro
```

Templates reference merge fields with Go template syntax:

```bash
-subject "Hi {{.first_name}}, a note for you"
-body    "<p>Hello {{.first_name}}, your plan: {{.plan}}.</p>"
```

## Dry run — ALWAYS do this before a real 1M blast

Renders every message and validates wiring, rate, and templates against the real
list, but sends **nothing**:

```bash
go run . -in users.csv -backend ses -from "News <news@you.com>" -dryrun -verbose
```

## Backends

### mock (default)
Logs sends. Zero deps. For load-testing the pipeline itself.

### smtp — send through your own VPS
```bash
go run . -in users.csv -backend smtp \
  -smtp-host mail.you.com -smtp-port 587 -smtp-tls \
  -smtp-user USER -smtp-pass PASS -from news@you.com
```
App running **on** the VPS next to Postfix on :25 (no auth):
```bash
go run . -in users.csv -backend smtp -smtp-host localhost -smtp-port 25 \
  -smtp-tls=false -from news@you.com -rate 100
```

**Connection pooling:** the SMTP backend keeps a bounded pool of persistent
connections and issues `RSET` between messages, so one TCP+STARTTLS+AUTH
handshake is amortized over many sends instead of paid per message — the single
biggest SMTP throughput win. Pool defaults to `-workers`; tune with `-smtp-pool`.
A connection that errors is dropped, never reused.

> **VPS deliverability reality:** a fresh VPS IP has zero reputation. Blast 1M
> cold = instant spam-folder + blacklist. To self-host you MUST:
> - **SPF + DKIM + DMARC** DNS records
> - **reverse DNS (PTR)** matching your HELO hostname
> - **warm the IP up** over days (ramp volume slowly)
> - handle bounces / complaints, keep the list clean
>
> Start rate low (`-rate 50`) and ramp. For 1M without owning deliverability,
> a reputable ESP is usually the pragmatic call.

### ses — Amazon SES (recommended for high volume)
Cheapest (~$0.10/1k ≈ $100 for 1M) and highest throughput after a quota bump.
Compiled behind a build tag so the default build stays dependency-free:

```bash
go build -tags ses -o emailblast .
AWS_REGION=us-east-1 ./emailblast -in users.csv -backend ses \
  -from "News <news@you.com>" -rate 500 -workers 300
```
Uses the standard AWS credential chain (env / shared config / IAM role).
Request an SES **sending quota increase** first — that quota is your throughput.

## Reliability features

- **Resume after crash** — completed IDs go to `checkpoint.log`; a re-run skips
  them. `-checkpoint ""` disables.
- **Dead-letter** — permanently failed recipients + cause go to `dead-letter.log`.
  Re-run against it later. `-dlq ""` disables.
- **Retry w/ exponential backoff** — throttling/5xx/network retried up to
  `-attempts` times; permanent errors (bad address) go straight to DLQ.
- **Idempotency key** — user ID travels as SES tag / SMTP `Message-ID` so a
  resend after crash is detectable downstream.
- **Graceful shutdown** — Ctrl-C / SIGTERM lets in-flight sends finish and
  flushes the checkpoint; re-run resumes cleanly.

## Logging

Structured logging via the stdlib `log/slog`. Two formats:

```bash
-log-format text   # human-friendly, default — key=value
-log-format json   # machine-parseable for Loki / CloudWatch / ELK aggregation
-log-level info    # debug | info | warn | error
```

`json` example (one object per line):

```json
{"time":"2026-07-23T16:35:21Z","level":"INFO","msg":"progress","sent":420000,"skipped":0,"failed":12,"retries":37}
{"time":"2026-07-23T16:35:21Z","level":"INFO","msg":"done","elapsed":"33m","sent":999988,"failed":12,"rate_per_sec":505}
```

The stdlib `log.Printf` calls inside the internal packages are bridged through
the same slog handler, so every line — operational events and incidental
warnings alike — shares one format and one stream (stderr). Per-recipient
failures are not logged individually at scale; they land in the dead-letter file
instead. Progress is logged every 2s.

## Flags

```
-in            input CSV (id,email + merge columns)      default users.csv
-backend       mock | smtp | ses                          default mock
-from          sender identity
-workers       concurrent send goroutines                 default 200
-rate          max sends/sec (ESP quota); 0 = unlimited   default 500
-attempts      max tries per recipient before DLQ         default 5
-checkpoint    resume file ("" disables)                  default checkpoint.log
-dlq           dead-letter file ("" disables)             default dead-letter.log
-dryrun        render + count, send NOTHING               default false
-subject       subject template
-body          HTML body template
-verbose       log every send
-smtp-host/-smtp-port/-smtp-user/-smtp-pass/-smtp-tls     SMTP backend
-smtp-pool     persistent connection pool size (0 = match -workers)
-log-format    text | json                                default text
-log-level     debug | info | warn | error                default info
-env           path to .env file                          default .env
-mock-delay/-mock-fail-every                              mock tuning
```

## Throughput math

At `-rate 500`, 1M emails ≈ **33 min**. Throughput is `min(rate, workers/latency)`
and capped by your ESP quota — raise the quota, then raise `-rate`. Adding workers
past `rate × latency` does nothing but queue.

## Scale beyond one node

For durable, multi-node sending (survive a machine dying mid-run), put the jobs
on **SQS/Kafka** instead of the in-process channel and run this worker pool on
many boxes — the `Sender`, checkpoint, and retry logic port unchanged.

## Build & test

```bash
go build ./...                 # default (mock + smtp), no external deps at runtime
go build -tags ses ./...       # adds Amazon SES backend
go vet ./...
```
