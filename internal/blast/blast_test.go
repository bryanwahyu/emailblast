package blast

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"emailblast/internal/checkpoint"
	"emailblast/internal/model"
	"emailblast/internal/render"
	"emailblast/internal/sender"
)

// --- retry classifier ---

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare sentinel", sender.ErrRetryable, true},
		{"wrapped sentinel", fmt.Errorf("MAIL FROM: %w", sender.ErrRetryable), true},
		{"double wrapped", fmt.Errorf("outer: %w", fmt.Errorf("%w: throttle", sender.ErrRetryable)), true},
		{"permanent", errors.New("550 bad recipient"), false},
	}
	for _, c := range cases {
		if got := isRetryable(c.err); got != c.want {
			t.Errorf("%s: isRetryable=%v want %v", c.name, got, c.want)
		}
	}
}

// spySender counts calls and returns a scripted error, for pipeline tests.
type spySender struct {
	calls atomic.Int64
	err   error
	delay time.Duration
}

func (s *spySender) Name() string { return "spy" }
func (s *spySender) Send(ctx context.Context, u model.User, m render.Message, key string) error {
	s.calls.Add(1)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.err
}

func newRunner(t *testing.T, cfg Config, snd sender.Sender) *Runner {
	t.Helper()
	r, err := render.New("s {{.name}}", "<p>{{.name}}</p>")
	if err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.Open("") // disabled
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(cfg, r, snd, cp)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func feed(users ...model.User) <-chan model.User {
	ch := make(chan model.User, len(users))
	for _, u := range users {
		ch <- u
	}
	close(ch)
	return ch
}

// --- rate limiter: throttling actually bounds throughput ---

func TestRateLimiterThrottles(t *testing.T) {
	spy := &spySender{}
	// 50/sec, burst 1 -> N sends take about (N-1)/50 seconds minimum.
	run := newRunner(t, Config{Workers: 8, RatePerSec: 50, Burst: 1, MaxAttempts: 1}, spy)

	const n = 15
	users := make([]model.User, n)
	for i := range users {
		users[i] = model.User{ID: fmt.Sprint(i), Email: fmt.Sprintf("u%d@x.com", i)}
	}

	start := time.Now()
	if err := run.Run(context.Background(), feed(users...)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	if got := spy.calls.Load(); got != n {
		t.Fatalf("sent %d want %d", got, n)
	}
	min := time.Duration(float64(n-1)/50*float64(time.Second)) - 30*time.Millisecond
	if elapsed < min {
		t.Errorf("finished in %s, faster than rate limit allows (~%s)", elapsed, min)
	}
}

// --- dedup: duplicate emails dropped, counted ---

func TestProducerDedup(t *testing.T) {
	spy := &spySender{}
	run := newRunner(t, Config{Workers: 4, RatePerSec: 0, MaxAttempts: 1}, spy)

	users := []model.User{
		{ID: "1", Email: "A@Ex.com "},
		{ID: "2", Email: "a@ex.com"}, // dup of 1 after normalize
		{ID: "3", Email: "c@ex.com"},
	}
	if err := run.Run(context.Background(), feed(users...)); err != nil {
		t.Fatal(err)
	}
	if got := run.Stats().Deduped.Load(); got != 1 {
		t.Errorf("deduped=%d want 1", got)
	}
	if got := run.Stats().Sent.Load(); got != 2 {
		t.Errorf("sent=%d want 2", got)
	}
}

// --- retry path: retryable error retried up to MaxAttempts then DLQ ---

func TestRetryThenDLQ(t *testing.T) {
	spy := &spySender{err: fmt.Errorf("%w: throttle", sender.ErrRetryable)}
	run := newRunner(t, Config{Workers: 1, RatePerSec: 0, MaxAttempts: 3, BaseBackoff: time.Millisecond}, spy)

	err := run.Run(context.Background(), feed(model.User{ID: "1", Email: "a@x.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := spy.calls.Load(); got != 3 {
		t.Errorf("attempts=%d want 3 (MaxAttempts)", got)
	}
	if got := run.Stats().Failed.Load(); got != 1 {
		t.Errorf("failed=%d want 1", got)
	}
	if got := run.Stats().Retries.Load(); got != 2 {
		t.Errorf("retries=%d want 2", got)
	}
}
