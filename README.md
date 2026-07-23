# emailblast

[![CI](https://github.com/bryanwahyu/emailblast/actions/workflows/ci.yml/badge.svg)](https://github.com/bryanwahyu/emailblast/actions/workflows/ci.yml)

Fast, resumable, personalized bulk email sender in Go. Built for **1M+ recipients**
on a single node using a bounded goroutine worker pool.

The real ceiling is your **ESP sending quota**, not Go. Goroutines are cheap;
the pool just hides network latency behind a shared rate limiter that respects
that quota.

## Safety

**By default this program sends no real email.** The default backend is `mock`,
which only logs what it would send. No message leaves the machine unless you
explicitly pass `-backend smtp` or `-backend ses` with real credentials. Before
any real blast, `-dryrun` wraps *any* backend to render and count against the
real recipient list while sending nothing. The 1M benchmark below ran entirely
on the `mock` backend — zero emails were delivered.

## Assumptions

- **Input** is a CSV with `id` and `email` columns; every other column is a
  personalization merge field. `id` is stable and unique (used as the
  idempotency / checkpoint key).
- **Delivery is at-least-once, not exactly-once.** A crash between send and
  checkpoint-flush can re-send a recipient; the idempotency key mitigates
  downstream but SES/SMTP have no native dedup.
- **The ESP sending quota is the throughput ceiling**, not Go. Worker count is
  sized to hide latency, not to exceed the quota.
- **Single node.** State lives in an in-process channel + a local checkpoint
  file; there is no cross-machine coordination.
- **Sender reputation, IP warmup, and DNS auth (SPF/DKIM/DMARC/PTR) are the
  operator's responsibility.** This tool sends; it does not manage deliverability.
- **`List-Unsubscribe` is required for real bulk sending** (Gmail/Yahoo rules);
  supply `-unsub-url` and/or `-unsub-mailto`. The program warns if unset.
- **Recipient emails may contain duplicates**; the producer dedups them
  (trim + lowercase) within a run and reports the count.
- **Bounce/complaint processing is out of scope** — wire SES SNS events or your
  MTA logs to a suppression list separately.
- **The benchmark uses a `mock` sender with simulated latency**; real-world rate
  is bounded by your ESP, not by the numbers here.
- **Clock/RNG-free core** where it matters: the CSV generator is deterministic so
  runs are reproducible.

## Why Go

Go is the right tool for this: **robust and fast** for high-concurrency I/O work.

- **Cheap concurrency** — goroutines cost ~2 KB each vs ~1 MB for an OS thread, so
  hundreds-to-thousands of concurrent senders are trivial. Email sending is
  I/O-bound (waiting on the network), so goroutines park cheaply while blocked
  instead of burning CPU.
- **Channels give backpressure for free** — the buffered jobs channel bounds
  memory without any external broker; the producer blocks when workers fall
  behind, so a 1M list never balloons RAM (measured ~270 MB peak).
- **Robust by construction** — static typing, `errgroup` for clean lifecycle +
  error propagation, `context` for graceful cancellation, and a single static
  binary (CGO off) that drops into any Alpine container with no runtime deps.
- **Fast in practice** — 47k sends/sec on a laptop in the benchmark below, with
  the real ceiling being the ESP quota, not the language.

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

## Internals — deep dive

### Concurrency model

One `errgroup.Group` (`blast.go:118`) owns every goroutine and a derived
`context`. Topology:

```
producer(1) ──chan model.Job (buffered, cap = workers×4)──▶ workers(N)
     │                                                          │
 CSV stream                                          limiter.Wait(ctx) ─▶ Sender.Send
```

- **Producer** (`blast.go`, one goroutine) owns the dedup set (a plain
  `map[string]struct{}`, no mutex — single writer) and the checkpoint skip check,
  then pushes `model.Job` onto the channel. It `select`s on `ctx.Done()` so a
  cancel unblocks a full-channel send instead of deadlocking. It `close()`s the
  channel on drain — the signal that lets workers `range` to completion.
- **Workers** (N goroutines) `for job := range jobs`. Channel receive is the
  synchronization point; no shared mutable state between workers except atomic
  counters (`Stats`). A worker returns a non-nil error **only** on context
  cancellation — per-recipient failures are swallowed into the DLQ so one bad
  address never collapses the pool via `errgroup`'s first-error cancel.
- **Cancellation** propagates through `ctx`: `errgroup` cancels on first error;
  SIGTERM/SIGINT cancel via `signal.NotifyContext` (`main.go`). `limiter.Wait`,
  channel sends, and backoff sleeps all `select` on `ctx.Done()`, so shutdown is
  prompt and in-flight sends drain rather than truncate.

### Backpressure & memory

The channel is bounded (`cap = workers×4`, `blast.go:61`). When workers can't keep
up, the buffer fills and the producer's send **blocks** — that is the
backpressure. Consequence: resident set is `O(workers)`, not `O(recipients)`.
Concretely, live memory ≈ `(buffered jobs + in-flight) × sizeof(User)` — a few MB
regardless of a 1M-row / 45 MB input (measured 270 MB peak, dominated by the Go
runtime + `html/template` buffers, not the list).

The CSV reader uses `ReuseRecord` (`csv.go`) — one backing array reused per row —
so parsing 1M rows allocates almost nothing per row. Field values are copied out
with `strings.Clone` precisely because the backing array is about to be
overwritten; without the clone, every `User` would alias freed bytes.

### Rate limiting — token bucket

`golang.org/x/time/rate.Limiter` (`blast.go:114`) is a token bucket: refills at
`Limit` tokens/sec, holds up to `Burst` at once. Every worker shares **one**
limiter, so the aggregate send rate across all N goroutines is globally capped —
the workers exist only to hide per-call latency, the limiter is the true throttle.
`limiter.Wait(ctx)` blocks until a token is free or `ctx` is done. `Burst`
defaults to `RatePerSec` (one second of slack); set `-rate 0` to remove the
limiter entirely (throughput then bounded by `workers / latency`). This is why
throughput is `min(rate, workers/latency)` capped by the ESP quota.

### Retry & error classification

`process()` (`blast.go`) renders once, then loops up to `MaxAttempts`. Errors are
classified, not blindly retried:

- **Retryable** (`isRetryable` → `errors.Is(err, sender.ErrRetryable)`): backends
  wrap transient faults — SES throttling / 5xx, SMTP 4xx on `RCPT`, any dial or
  network error — with `fmt.Errorf("%w: …", ErrRetryable)`. `errors.Is` unwraps
  the chain, so nested wraps still classify correctly.
- **Permanent** (bad address, SMTP 5xx, auth failure, a render error): straight to
  DLQ, no retry — retrying can't fix them and would waste quota.

Backoff is exponential: `200ms → 400ms → 800ms …` (`backoff *= 2`), each sleep
`select`ing on `ctx`. On exhaustion or a permanent error the ID + cause append to
the dead-letter file under a mutex.

### Checkpoint durability & the duplicate window

`checkpoint.log` is an append-only newline list of completed IDs, fronted by an
in-memory `map[string]struct{}` for O(1) skip checks. On startup the file is
loaded into the set (`Open`), giving resume. Writes go through a `bufio.Writer`.

The subtle part is the **crash semantics**:

- `bufio.Flush()` (called every 2s by the progress goroutine, `main.go:257`)
  pushes buffered bytes into the OS. A `kill -9` of the *process* does **not** lose
  flushed data — the page cache belongs to the kernel, not the process.
- What a hard kill *can* lose is the bytes still sitting in the in-process `bufio`
  buffer since the last flush. So the **duplicate window is bounded by the flush
  interval**: at rate `R`, at most `~2s × R` recipients could be re-sent on resume.
- That is deliberately at-least-once, not exactly-once — the idempotency key
  (`Message-ID` / SES tag) lets a downstream layer dedup if it must. Full fsync
  per record would make it tighter but throttle throughput to disk-sync speed;
  the 2s window is the chosen trade. The benchmark's crash test hit this path and
  resumed with **zero** duplicates because a flush had landed before the kill.

### SMTP connection pooling — cost model

Per-message SMTP cost without pooling is dominated by the handshake: TCP + STARTTLS
(a TLS handshake ≈ 1–2 RTT) + AUTH — easily 100–300 ms before a single byte of
mail. `SMTPSender` (`smtp.go`) keeps a buffered channel of live `*smtp.Client`;
`getConn` pulls an idle one (handshake already paid) or dials, and after a
successful `DATA` it issues `RSET` and returns the connection to the pool. So the
handshake is amortized: `total ≈ handshake × pool_size + per_msg_txn × messages`
instead of `(handshake + per_msg_txn) × messages`. A connection that errors is
`Close()`d and dropped, never returned — one broken link can't poison the pool.
Pool size defaults to `-workers` so every worker can hold a warm connection.

### NATS JetStream — concrete multi-node design

Swapping the in-process channel for NATS makes the pool horizontally scalable:

- **Stream**: `EMAILS`, subject `emails.jobs.*`, `FileStorage`, retention
  `WorkQueue` (a message is removed once acked — natural once-delivery within the
  cluster).
- **Producer**: the CSV reader publishes one message per recipient with header
  `Nats-Msg-Id: <user.ID>`. JetStream's dedup window drops duplicate publishes,
  so a producer retry can't double-enqueue.
- **Consumers**: a **pull** consumer with `AckExplicit`, `MaxAckPending` ≈ the node's
  worker count, and `AckWait` > worst-case send-time. Each node runs *this exact
  pool*; workers `Fetch` a batch, send, then `msg.Ack()` — **`Ack` replaces
  `checkpoint.Done`**. A node crash means unacked messages redeliver elsewhere
  after `AckWait` (`MaxDeliver` caps attempts → terminal `msg.Term()` = DLQ).
- **What changes**: only the producer (CSV → publish) and the sink
  (`checkpoint.Done` → `msg.Ack`). The `Sender`, shared rate limiter, retry
  classifier, and DLQ logic port **unchanged** — the rate limiter now throttles
  per node, so set it to `quota / node_count`.

## Components

| File | Role |
|------|------|
| `internal/source/csv.go` | Streams recipients lazily (1M rows never in RAM) |
| `internal/render/render.go` | Parse-once subject (`text/template`) + body (`html/template`, auto-escaped), concurrent-safe |
| `internal/blast/blast.go` | Worker pool, rate limiter, retry/backoff, DLQ |
| `internal/checkpoint/checkpoint.go` | Resumable progress log (crash-safe) |
| `internal/sender/sender.go` | `Sender` interface + `mock` backend (default) |
| `internal/sender/smtp.go` | SMTP backend (**pooled** persistent conns) — your **VPS** / relay |
| `internal/sender/ses.go` | Amazon SES v2 backend (build tag `ses`) |
| `internal/sender/dryrun.go` | Wraps any backend — renders + counts, sends nothing |
| `internal/config/dotenv.go` | Minimal `.env` loader (no dependency) |
| `cmd/gencsv/` | Deterministic 1M-row test CSV generator |

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
- **HTML-safe personalization** — the body is `html/template` (subject is
  `text/template`), so a merge value like `O'Brien & <Sons>` is context-escaped,
  not injected. Covered by a unit test.
- **Email dedup** — the producer drops duplicate addresses (trim + lowercase)
  within a run and reports the `deduped` count in the done log.
- **List-Unsubscribe** — `-unsub-url` / `-unsub-mailto` add `List-Unsubscribe`
  (+ `List-Unsubscribe-Post: List-Unsubscribe=One-Click`, RFC 8058) to SMTP and
  SES. Required by Gmail/Yahoo bulk-sender rules; the program warns if unset.

## Tests

```bash
go test -race ./...
```

Five focused suites: **render** (HTML escaping of `O'Brien & <Sons>`, missing
keys), **retry classifier** (`errors.Is` unwrapping of retryable vs permanent),
**rate limiter** (throughput is actually throttled), **checkpoint** (resume
reload + flush-without-close durability), and **dry-run** (inner backend never
called). Plus dedup and List-Unsubscribe header construction. CI runs
build + vet + test on every push.

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
-unsub-url     List-Unsubscribe URL ({{email}} substituted; enables one-click)
-unsub-mailto  List-Unsubscribe mailto address
-log-format    text | json                                default text
-log-level     debug | info | warn | error                default info
-env           path to .env file                          default .env
-mock-delay/-mock-fail-every                              mock tuning
```

## Throughput math

At `-rate 500`, 1M emails ≈ **33 min**. Throughput is `min(rate, workers/latency)`
and capped by your ESP quota — raise the quota, then raise `-rate`. Adding workers
past `rate × latency` does nothing but queue.

## Benchmark (measured)

Real runs on the `mock` backend with a simulated 20ms per-send latency — **no
real email sent**. Recipient list generated by `go run ./cmd/gencsv -n 1000000`.
Raw evidence: [`bench/results/benchmark_raw.txt`](bench/results/benchmark_raw.txt).

**Machine:** Intel Core i5-1038NG7 (4 cores / 8 threads), 16 GB RAM, macOS
(darwin 25.5), Go 1.25.5.

| Run | Recipients | Config | Elapsed | Rate | Peak RSS | Result |
|-----|-----------:|--------|--------:|-----:|---------:|--------|
| **1 — full** | 1,000,000 | 1000 workers, 20ms latency, rate=0 | **21.1s** | **47,287/s** | **270 MB** | sent 1,000,000 / failed 0 |
| **2a — crash** | (killed) | 500 workers, `kill -9` mid-run | — | — | — | checkpoint at 446,574 sent |
| **2b — resume** | remainder | same checkpoint, rerun | 23.6s | 23,424/s | 268 MB | skipped **446,574**, sent **553,426** → **1,000,000 total** |

**Two things this proves:**

1. **Memory is bounded by streaming, not by list size** — peak RSS stayed ~270 MB
   for a 1M-row (45 MB) input; nothing loads the full list into RAM.
2. **Checkpoint resume is exact** — after a hard `kill -9`, the rerun skipped
   *precisely* the 446,574 already sent and completed the remaining 553,426.
   446,574 + 553,426 = 1,000,000, no loss, no duplicates.

> Throughput here is gated by the artificial 20ms latency and worker count, not a
> real ESP. On a real backend, `min(rate, workers/latency)` capped by your ESP
> quota governs — raise the quota, then `-rate`.

## Scale beyond one node

Today it's single-node: the jobs channel lives in-process. To go multi-node
(survive a machine dying mid-run, spread load across boxes), swap that channel
for a distributed queue. **My pick is [NATS](https://nats.io) (JetStream):**

- **Fast + lightweight** — a single Go binary, millions of msgs/sec, tiny
  footprint; fits the same "robust and fast" reasoning as the app itself.
- **JetStream durability** — persistent streams + acks give at-least-once
  delivery and redelivery on consumer crash, replacing the local checkpoint's
  role for cross-machine work.
- **Consumer groups** — many worker nodes pull from one stream and NATS balances
  the work; each node runs this exact pool, just reading jobs from NATS instead
  of the CSV producer.

Only the producer (CSV → channel) and the sink (`checkpoint.Done`) change; the
`Sender`, rate limiter, retry/backoff, and DLQ logic port unchanged. (SQS/Kafka
work too — NATS is the leanest option here.)

## Build & test

```bash
go build ./...                 # default (mock + smtp), no external deps at runtime
go build -tags ses ./...       # adds Amazon SES backend
go vet ./...
```
