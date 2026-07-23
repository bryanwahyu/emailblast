package sender

import (
	"context"
	"log"
	"sync/atomic"

	"emailblast/internal/model"
	"emailblast/internal/render"
)

// DryRun wraps any Sender and short-circuits the actual delivery: it renders and
// counts but never touches the network. Use it to validate templates, rate,
// checkpoint/resume, and ESP wiring against the real recipient list before
// spending real sends — critical safety net before a 1M blast.
//
// It still reports the wrapped backend's name (suffixed) so logs make the mode
// obvious.
type DryRun struct {
	inner Sender
	log   bool
	count atomic.Int64
}

func NewDryRun(inner Sender, logEach bool) *DryRun {
	return &DryRun{inner: inner, log: logEach}
}

func (d *DryRun) Name() string { return d.inner.Name() + "+dryrun" }

func (d *DryRun) Send(ctx context.Context, u model.User, msg render.Message, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n := d.count.Add(1)
	if d.log {
		log.Printf("[DRYRUN] would send #%d to=%s key=%s subject=%q", n, u.Email, key, msg.Subject)
	}
	return nil // never delivers
}

// Count is how many sends were simulated.
func (d *DryRun) Count() int64 { return d.count.Load() }
