package sender

import (
	"math/rand"
	"time"
)

// Backoff computes per-entry retry delays: exponential with full decorrelated
// jitter, bounded by Max. Attempt counts start at 1.
type Backoff struct {
	Min time.Duration
	Max time.Duration
}

// Delay returns the wait before retry number `attempt` (1-based).
func (b Backoff) Delay(attempt uint32) time.Duration {
	min := b.Min
	if min <= 0 {
		min = time.Second
	}
	max := b.Max
	if max <= 0 {
		max = 5 * time.Minute
	}

	backoff := min << (attempt - 1)
	if attempt > 20 || backoff > max || backoff <= 0 { // shift overflow guard
		backoff = max
	}
	// Jitter in [backoff/2, backoff): retries from many entries spread out
	// instead of stampeding the RPC endpoint together.
	half := backoff / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}
