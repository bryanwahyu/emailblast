// Package sender abstracts the email service provider (ESP). The worker pool
// depends only on the Sender interface, so SES / SendGrid / a mock are
// swappable without touching pipeline code.
package sender

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"emailblast/internal/model"
	"emailblast/internal/render"
)

// Sender delivers one rendered message to one recipient.
//
// Implementations must be safe for concurrent use by many goroutines.
// The idempotencyKey (user ID) lets the ESP or a downstream dedupe layer
// discard duplicate deliveries after a crash-and-resume.
type Sender interface {
	Send(ctx context.Context, u model.User, msg render.Message, idempotencyKey string) error
	Name() string
}

// ErrRetryable marks a failure the pool should retry (throttling, 5xx, timeout)
// as opposed to a permanent failure (invalid address) which is dropped to DLQ.
var ErrRetryable = errors.New("retryable send failure")

// MockSender writes deliveries to the log instead of hitting a real ESP. Default
// backend so the program builds and runs with zero external dependencies.
//
// failEvery > 0 injects a retryable failure on every Nth call to exercise the
// retry path. delay simulates network latency so the concurrency is observable.
type MockSender struct {
	failEvery int64
	delay     time.Duration
	verbose   bool
	count     atomic.Int64
	sent      atomic.Int64
}

func NewMock(failEvery int64, delay time.Duration, verbose bool) *MockSender {
	return &MockSender{failEvery: failEvery, delay: delay, verbose: verbose}
}

func (m *MockSender) Name() string { return "mock" }

func (m *MockSender) Send(ctx context.Context, u model.User, msg render.Message, key string) error {
	n := m.count.Add(1)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.failEvery > 0 && n%m.failEvery == 0 {
		return ErrRetryable // simulate throttle/5xx
	}
	m.sent.Add(1)
	if m.verbose {
		log.Printf("SEND to=%s key=%s subject=%q", u.Email, key, msg.Subject)
	}
	return nil
}

// Sent reports how many messages were actually delivered (excludes simulated
// failures). Useful for the final summary.
func (m *MockSender) Sent() int64 { return m.sent.Load() }
