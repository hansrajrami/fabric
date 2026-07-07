package sender

import (
	"fmt"
	"time"
)

// CadenceMode selects when pending commitments are flushed to MST
// (spec section 12.1). Capture always writes to the outbox at commit;
// cadence affects only WHEN entries are anchored, never WHETHER.
type CadenceMode string

const (
	// CadencePerTx submits each commitment as soon as it is captured.
	CadencePerTx CadenceMode = "per-tx"
	// CadenceBatch waits until at least N commitments are pending, then
	// flushes them all (age-capped by MaxWait).
	CadenceBatch CadenceMode = "batch"
	// CadenceInterval flushes on a fixed schedule.
	CadenceInterval CadenceMode = "interval"
)

// Cadence is the flush policy.
type Cadence struct {
	Mode CadenceMode
	// N is the batch threshold for CadenceBatch.
	N int
	// Interval is the flush period for CadenceInterval.
	Interval time.Duration
	// MaxWait caps how long a pending entry can sit under CadenceBatch even
	// if the threshold is never reached (default 1 minute). Guarantees
	// progress when traffic stops just below N.
	MaxWait time.Duration
}

// Validate normalizes and checks the policy.
func (c *Cadence) Validate() error {
	switch c.Mode {
	case "", CadencePerTx:
		c.Mode = CadencePerTx
	case CadenceBatch:
		if c.N <= 0 {
			return fmt.Errorf("sender: batch cadence requires N > 0")
		}
		if c.MaxWait <= 0 {
			c.MaxWait = time.Minute
		}
	case CadenceInterval:
		if c.Interval <= 0 {
			return fmt.Errorf("sender: interval cadence requires a positive interval")
		}
	default:
		return fmt.Errorf("sender: unknown cadence mode %q", c.Mode)
	}
	return nil
}

// pollInterval is how often the sender re-examines the outbox. Per-tx uses a
// tight poll for low latency; interval mode polls at its own period.
func (c *Cadence) pollInterval(base time.Duration) time.Duration {
	if c.Mode == CadenceInterval {
		return c.Interval
	}
	return base
}

// shouldFlush decides whether to flush now given the pending count and the
// age of the oldest pending entry.
func (c *Cadence) shouldFlush(pending int, oldestAge time.Duration) bool {
	if pending == 0 {
		return false
	}
	switch c.Mode {
	case CadenceBatch:
		return pending >= c.N || oldestAge >= c.MaxWait
	default: // per-tx and interval flush whatever is due when they wake
		return true
	}
}
