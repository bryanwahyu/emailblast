// Package checkpoint records which user IDs were already sent so a crashed run
// can resume without re-emailing a million people. It is an append-only log of
// completed IDs plus an in-memory set for O(1) skip checks.
package checkpoint

import (
	"bufio"
	"fmt"
	"os"
	"sync"
)

// Checkpoint is concurrency-safe: workers call Done from many goroutines.
type Checkpoint struct {
	mu   sync.Mutex
	w    *bufio.Writer
	f    *os.File
	done map[string]struct{}
}

// Open loads any existing checkpoint file into the skip set, then opens it for
// appending. Passing "" disables checkpointing (Has always false, Done no-op).
func Open(path string) (*Checkpoint, error) {
	c := &Checkpoint{done: make(map[string]struct{})}
	if path == "" {
		return c, nil
	}

	// Load prior progress if the file exists.
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if id := sc.Text(); id != "" {
				c.done[id] = struct{}{}
			}
		}
		f.Close()
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open checkpoint: %w", err)
	}
	c.f = f
	c.w = bufio.NewWriter(f)
	return c, nil
}

// Has reports whether id was completed in a previous run (skip it).
func (c *Checkpoint) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.done[id]
	return ok
}

// Done records a successful send durably. Buffered; Flush/Close persists.
func (c *Checkpoint) Done(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done[id] = struct{}{}
	if c.w == nil {
		return nil
	}
	_, err := c.w.WriteString(id + "\n")
	return err
}

// Flush persists buffered records to the OS without closing. Call it
// periodically during a long run so a hard crash (kill -9) loses at most the
// sends since the last flush, bounding duplicate re-sends on resume.
func (c *Checkpoint) Flush() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		return nil
	}
	return c.w.Flush()
}

// Count returns how many IDs are marked done.
func (c *Checkpoint) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.done)
}

// Close flushes buffered records and closes the file.
func (c *Checkpoint) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w != nil {
		if err := c.w.Flush(); err != nil {
			return err
		}
	}
	if c.f != nil {
		return c.f.Close()
	}
	return nil
}
