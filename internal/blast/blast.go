// Package blast is the concurrent send pipeline: one producer goroutine streams
// users into a buffered channel, a fixed pool of worker goroutines renders and
// sends each one under a shared rate limiter, and failures retry with backoff
// before landing in a dead-letter file.
//
// Design notes:
//   - Fixed worker count, NOT one goroutine per user. 1M goroutines would flood
//     the ESP and blow memory; the ESP quota is the real ceiling, so N workers
//     ~= (quota * latency) is enough to saturate it.
//   - Buffered jobs channel gives backpressure: the producer blocks when workers
//     fall behind, so RAM stays bounded regardless of input size.
//   - rate.Limiter is the true throttle. Workers only exist to hide network
//     latency behind the limiter.
package blast

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"emailblast/internal/checkpoint"
	"emailblast/internal/model"
	"emailblast/internal/render"
	"emailblast/internal/sender"
)

// Config tunes the pipeline. Zero values are replaced with sane defaults.
type Config struct {
	Workers     int           // concurrent send goroutines
	RatePerSec  int           // ESP-facing rate limit (msgs/sec); <=0 means unlimited
	Burst       int           // limiter burst; defaults to RatePerSec
	MaxAttempts int           // total tries per user before DLQ (>=1)
	BaseBackoff time.Duration // first retry delay; doubles each attempt
	QueueDepth  int           // jobs channel buffer
	DLQPath     string        // dead-letter file for permanently failed IDs
}

func (c *Config) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 200
	}
	if c.Burst <= 0 {
		c.Burst = c.RatePerSec
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 200 * time.Millisecond
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = c.Workers * 4
	}
}

// Stats is the outcome summary, all counters updated atomically.
type Stats struct {
	Sent    atomic.Int64
	Skipped atomic.Int64 // already done per checkpoint
	Deduped atomic.Int64 // dropped as duplicate email within this run
	Failed  atomic.Int64 // exhausted retries -> DLQ
	Retries atomic.Int64
}

// Runner wires the collaborators together.
type Runner struct {
	cfg   Config
	rend  *render.Renderer
	send  sender.Sender
	cp    *checkpoint.Checkpoint
	stats Stats

	dlqMu sync.Mutex
	dlq   *os.File
}

func New(cfg Config, r *render.Renderer, s sender.Sender, cp *checkpoint.Checkpoint) (*Runner, error) {
	cfg.applyDefaults()
	run := &Runner{cfg: cfg, rend: r, send: s, cp: cp}
	if cfg.DLQPath != "" {
		f, err := os.OpenFile(cfg.DLQPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open dlq: %w", err)
		}
		run.dlq = f
	}
	return run, nil
}

// Stats exposes the live counters.
func (r *Runner) Stats() *Stats { return &r.stats }

// Run streams from users, drives the pool, and blocks until the input is
// drained and every in-flight job settles (or ctx is cancelled). The users
// channel must be produced by a separate goroutine; Run does not close it.
func (r *Runner) Run(ctx context.Context, users <-chan model.User) error {
	defer func() {
		if r.dlq != nil {
			r.dlq.Close()
		}
	}()

	var limiter *rate.Limiter
	if r.cfg.RatePerSec > 0 {
		limiter = rate.NewLimiter(rate.Limit(r.cfg.RatePerSec), r.cfg.Burst)
	}

	jobs := make(chan model.Job, r.cfg.QueueDepth)
	g, ctx := errgroup.WithContext(ctx)

	// Producer: adapt the user stream into jobs, honoring the checkpoint so a
	// resumed run skips already-sent IDs cheaply, and dropping duplicate email
	// addresses (normalized: trimmed + lowercased) so nobody is mailed twice
	// within a run. Dedup lives in the single producer goroutine, so the set
	// needs no lock.
	g.Go(func() error {
		defer close(jobs)
		seen := make(map[string]struct{})
		for u := range users {
			norm := strings.ToLower(strings.TrimSpace(u.Email))
			if _, dup := seen[norm]; dup {
				r.stats.Deduped.Add(1)
				continue
			}
			seen[norm] = struct{}{}
			if r.cp.Has(u.ID) {
				r.stats.Skipped.Add(1)
				continue
			}
			select {
			case jobs <- model.Job{User: u}:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})

	// Worker pool.
	for i := 0; i < r.cfg.Workers; i++ {
		g.Go(func() error {
			for job := range jobs {
				if err := r.process(ctx, limiter, job); err != nil {
					return err // only ctx cancellation escapes; send failures are handled inside
				}
			}
			return nil
		})
	}

	return g.Wait()
}

// process handles one user end-to-end: render once, then send with bounded
// retries + exponential backoff. Returns non-nil ONLY on context cancellation
// so a single bad recipient never tears down the pool.
func (r *Runner) process(ctx context.Context, limiter *rate.Limiter, job model.Job) error {
	msg, err := r.rend.Render(job.User)
	if err != nil {
		// Rendering is deterministic; a failure is permanent -> DLQ, no retry.
		r.toDLQ(job.User.ID, err)
		return nil
	}

	backoff := r.cfg.BaseBackoff
	for attempt := 1; attempt <= r.cfg.MaxAttempts; attempt++ {
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return err // ctx done
			}
		}

		err := r.send.Send(ctx, job.User, msg, job.User.ID)
		if err == nil {
			r.stats.Sent.Add(1)
			if cpErr := r.cp.Done(job.User.ID); cpErr != nil {
				log.Printf("checkpoint write failed for %s: %v", job.User.ID, cpErr)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isRetryable(err) || attempt == r.cfg.MaxAttempts {
			r.toDLQ(job.User.ID, err)
			return nil
		}

		r.stats.Retries.Add(1)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2 // exponential
	}
	return nil
}

func isRetryable(err error) bool {
	// sender.ErrRetryable is wrapped by ESP backends (fmt.Errorf("%w: ...")).
	return errors.Is(err, sender.ErrRetryable)
}

// toDLQ records a permanently failed ID and bumps the counter. The pipeline
// keeps running; the operator can re-run against the DLQ file later.
func (r *Runner) toDLQ(id string, cause error) {
	r.stats.Failed.Add(1)
	if r.dlq == nil {
		log.Printf("DLQ %s: %v", id, cause)
		return
	}
	r.dlqMu.Lock()
	fmt.Fprintf(r.dlq, "%s\t%v\n", id, cause)
	r.dlqMu.Unlock()
}
