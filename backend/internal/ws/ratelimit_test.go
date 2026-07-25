package ws

import (
	"testing"
	"time"
)

func TestTokenBucketAllow(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		rate       float64
		burst      float64
		offsets    []time.Duration // one entry per allow() call
		wantAllows []bool
	}{
		{
			name:       "burst is spendable immediately",
			rate:       5,
			burst:      3,
			offsets:    []time.Duration{0, 0, 0, 0},
			wantAllows: []bool{true, true, true, false},
		},
		{
			name:       "refills at the configured rate",
			rate:       5, // one token every 200ms
			burst:      1,
			offsets:    []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond},
			wantAllows: []bool{true, false, true},
		},
		{
			name:       "refill is capped at burst",
			rate:       5,
			burst:      2,
			offsets:    []time.Duration{0, 0, time.Hour, time.Hour, time.Hour},
			wantAllows: []bool{true, true, true, true, false},
		},
		{
			name:       "clock going backwards neither refills nor drains",
			rate:       5,
			burst:      2,
			offsets:    []time.Duration{0, -time.Hour, -time.Hour},
			wantAllows: []bool{true, true, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newTokenBucket(tt.rate, tt.burst, base)
			for i, off := range tt.offsets {
				if got := b.allow(base.Add(off)); got != tt.wantAllows[i] {
					t.Errorf("allow #%d at %v = %v, want %v", i, off, got, tt.wantAllows[i])
				}
			}
		})
	}
}

func TestTokenBucketConcurrent(t *testing.T) {
	now := time.Now()
	b := newTokenBucket(0, 100, now) // no refill: exactly 100 allowances exist

	const goroutines = 20
	results := make(chan bool, goroutines*10)
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				results <- b.allow(now)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	close(results)

	allowed := 0
	for ok := range results {
		if ok {
			allowed++
		}
	}
	if allowed != 100 {
		t.Fatalf("allowed = %d, want exactly 100", allowed)
	}
}
