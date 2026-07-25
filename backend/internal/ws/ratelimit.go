package ws

import (
	"sync"
	"time"
)

// tokenBucket is a per-connection inbound frame limiter.
//
// The HTTP rate limiter only ever sees the single upgrade request, yet every
// subscribe/typing frame afterwards costs a Postgres round-trip inside the read
// loop. This bounds that: a steady rate of `rate` frames per second with a
// `burst` allowance for the resubscribe storm a client emits on reconnect.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	rate   float64 // tokens added per second
	burst  float64 // maximum tokens held
	last   time.Time
}

func newTokenBucket(rate, burst float64, now time.Time) *tokenBucket {
	return &tokenBucket{tokens: burst, rate: rate, burst: burst, last: now}
}

// allow consumes one token, refilling first for the time elapsed since the last
// call. A non-monotonic `now` (clock adjustment) neither refills nor drains.
func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
