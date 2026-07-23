// Command emailblast sends personalized email to a large recipient list using a
// bounded goroutine worker pool, a shared rate limiter, retry-with-backoff, a
// resumable checkpoint, and a dead-letter file.
//
// Quick start (no AWS, no SMTP server needed — logs sends):
//
//	go run . -in users.csv -backend mock -workers 200 -rate 500 -verbose
//
// Real send via Amazon SES (needs the ses build tag + AWS creds):
//
//	go build -tags ses -o emailblast .
//	./emailblast -in users.csv -backend ses -from "News <news@you.com>" -rate 500
//
// Real send via your own VPS mail server / SMTP relay:
//
//	./emailblast -in users.csv -backend smtp -smtp-host mail.you.com \
//	    -smtp-port 587 -from news@you.com -smtp-user u -smtp-pass p -smtp-tls
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"emailblast/internal/blast"
	"emailblast/internal/checkpoint"
	"emailblast/internal/config"
	"emailblast/internal/model"
	"emailblast/internal/render"
	"emailblast/internal/sender"
)

// env* helpers give each flag a default sourced from the environment (populated
// from .env), falling back to the literal default. Keeps secrets off the CLI.
func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func main() {
	// Load .env (optional) BEFORE flags so env-backed defaults are populated.
	// Scan os.Args directly since flag.Parse hasn't run yet; handle both
	// "-env path", "--env path" and "-env=path", "--env=path".
	envPath := ".env"
	for i, a := range os.Args {
		switch {
		case a == "-env" || a == "--env":
			if i+1 < len(os.Args) {
				envPath = os.Args[i+1]
			}
		case strings.HasPrefix(a, "-env="):
			envPath = strings.TrimPrefix(a, "-env=")
		case strings.HasPrefix(a, "--env="):
			envPath = strings.TrimPrefix(a, "--env=")
		}
	}
	if err := config.LoadDotenv(envPath); err != nil {
		log.Fatalf("load .env: %v", err)
	}

	var (
		_       = flag.String("env", ".env", "path to .env file (loaded before flags)")
		in      = flag.String("in", envStr("EMAIL_IN", "users.csv"), "input CSV (must have id,email columns)")
		backend = flag.String("backend", envStr("EMAIL_BACKEND", "mock"), "esp backend: mock | smtp | ses")
		from    = flag.String("from", envStr("EMAIL_FROM", "no-reply@example.com"), "sender identity")

		workers  = flag.Int("workers", envInt("EMAIL_WORKERS", 200), "concurrent send goroutines")
		ratePerS = flag.Int("rate", envInt("EMAIL_RATE", 500), "max sends/sec (ESP quota); 0 = unlimited")
		attempts = flag.Int("attempts", envInt("EMAIL_ATTEMPTS", 5), "max tries per recipient before DLQ")

		cpPath  = flag.String("checkpoint", envStr("EMAIL_CHECKPOINT", "checkpoint.log"), "resume file; empty disables")
		dlqPath = flag.String("dlq", envStr("EMAIL_DLQ", "dead-letter.log"), "dead-letter file; empty disables")
		verbose = flag.Bool("verbose", envBool("EMAIL_VERBOSE", false), "log every send (mock backend)")
		dryRun  = flag.Bool("dryrun", envBool("EMAIL_DRYRUN", false), "render + count but send NOTHING (wraps any backend)")

		subjectTmpl = flag.String("subject", envStr("EMAIL_SUBJECT", "Hi {{.first_name}}, a note for you"), "subject template")
		bodyTmpl    = flag.String("body", envStr("EMAIL_BODY", "<p>Hello {{.first_name}},</p><p>Your plan: {{.plan}}.</p>"), "HTML body template")

		// mock knobs
		mockDelay = flag.Duration("mock-delay", envDur("EMAIL_MOCK_DELAY", 2*time.Millisecond), "simulated latency (mock)")
		mockFail  = flag.Int64("mock-fail-every", int64(envInt("EMAIL_MOCK_FAIL_EVERY", 0)), "inject retryable failure every Nth send (mock); 0 disables")

		// smtp knobs
		smtpHost = flag.String("smtp-host", envStr("SMTP_HOST", ""), "SMTP host")
		smtpPort = flag.String("smtp-port", envStr("SMTP_PORT", "587"), "SMTP port")
		smtpUser = flag.String("smtp-user", envStr("SMTP_USER", ""), "SMTP username (empty = no auth)")
		smtpPass = flag.String("smtp-pass", envStr("SMTP_PASS", ""), "SMTP password")
		smtpTLS  = flag.Bool("smtp-tls", envBool("SMTP_TLS", true), "use STARTTLS")
		smtpPool = flag.Int("smtp-pool", envInt("SMTP_POOL", 0), "SMTP connection pool size (0 = match -workers)")

		logFormat = flag.String("log-format", envStr("EMAIL_LOG_FORMAT", "text"), "log format: text | json")
		logLevel  = flag.String("log-level", envStr("EMAIL_LOG_LEVEL", "info"), "log level: debug | info | warn | error")
	)
	flag.Parse()

	// Structured logging via slog. json = machine-parseable for aggregation
	// (Loki/CloudWatch/ELK) on a real 1M run; text = human-friendly locally.
	// slog.SetDefault also routes the stdlib log.Printf calls inside the
	// internal packages through the same handler, so ALL output is uniform.
	logger := newLogger(*logFormat, *logLevel)
	slog.SetDefault(logger)
	// Route the stdlib log.Printf calls inside internal packages through the
	// same slog handler, so ALL output is uniform (one format, one stream).
	log.SetFlags(0)
	log.SetOutput(slog.NewLogLogger(logger.Handler(), slog.LevelWarn).Writer())

	// Ctrl-C / SIGTERM cancels gracefully: in-flight sends finish, the
	// checkpoint flushes, and a re-run resumes where this left off.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rend, err := render.New(*subjectTmpl, *bodyTmpl)
	if err != nil {
		logger.Error("template", "err", err)
		os.Exit(1)
	}

	// Default the SMTP pool to the worker count so every worker can hold a
	// warm connection instead of contending.
	poolSize := *smtpPool
	if poolSize <= 0 {
		poolSize = *workers
	}
	snd, err := buildSender(ctx, *backend, *from, *verbose, *mockDelay, *mockFail,
		*smtpHost, *smtpPort, *smtpUser, *smtpPass, *smtpTLS, poolSize)
	if err != nil {
		logger.Error("backend", "err", err)
		os.Exit(1)
	}
	if *dryRun {
		snd = sender.NewDryRun(snd, *verbose)
		logger.Warn("dry run active: no email will actually be sent")
	}

	cp, err := checkpoint.Open(*cpPath)
	if err != nil {
		logger.Error("checkpoint", "err", err)
		os.Exit(1)
	}
	defer cp.Close()
	if n := cp.Count(); n > 0 {
		logger.Info("resuming from checkpoint", "already_sent", n)
	}

	runner, err := blast.New(blast.Config{
		Workers:     *workers,
		RatePerSec:  *ratePerS,
		MaxAttempts: *attempts,
		DLQPath:     *dlqPath,
	}, rend, snd, cp)
	if err != nil {
		logger.Error("runner", "err", err)
		os.Exit(1)
	}

	// Producer streams users from the source into a channel.
	users := make(chan model.User, *workers*4)
	src := sourceStream(*in, users)

	// Periodic progress line + checkpoint flush (bounds duplicate re-sends if
	// the process is hard-killed between flushes).
	done := make(chan struct{})
	go progress(logger, runner.Stats(), cp, done)

	logger.Info("starting blast", "backend", snd.Name(), "workers", *workers, "rate_per_sec", *ratePerS)
	start := time.Now()

	runErr := runner.Run(ctx, users)
	close(done)

	if srcErr := <-src; srcErr != nil {
		logger.Error("source error", "err", srcErr)
	}
	if runErr != nil {
		logger.Warn("run ended early", "reason", runErr)
	}

	st := runner.Stats()
	elapsed := time.Since(start)
	logger.Info("done",
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"sent", st.Sent.Load(),
		"skipped", st.Skipped.Load(),
		"failed", st.Failed.Load(),
		"retries", st.Retries.Load(),
		"rate_per_sec", int(float64(st.Sent.Load())/elapsed.Seconds()),
	)
}

// newLogger builds a slog.Logger writing to stderr in text or json.
func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// progress logs a live counter and flushes the checkpoint every 2s until done
// is closed.
func progress(logger *slog.Logger, st *blast.Stats, cp *checkpoint.Checkpoint, done <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := cp.Flush(); err != nil {
				logger.Warn("checkpoint flush failed", "err", err)
			}
			logger.Info("progress",
				"sent", st.Sent.Load(),
				"skipped", st.Skipped.Load(),
				"failed", st.Failed.Load(),
				"retries", st.Retries.Load())
		}
	}
}
