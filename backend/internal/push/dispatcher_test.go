package push

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeSender records the batches it is handed and can be made to block, which
// is how the queue is filled deterministically.
type fakeSender struct {
	mu      sync.Mutex
	batches [][]Message

	// block, when non-nil, holds every Send until it is closed.
	block chan struct{}
	err   error
}

func (f *fakeSender) Send(_ context.Context, msgs []Message) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]Message(nil), msgs...))
	return f.err
}

func (f *fakeSender) snapshot() [][]Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]Message(nil), f.batches...)
}

func (f *fakeSender) total() int {
	n := 0
	for _, b := range f.snapshot() {
		n += len(b)
	}
	return n
}

func token(i int) string { return fmt.Sprintf("ExponentPushToken[t%03d]", i) }

func enqueueN(d *Dispatcher, n int) {
	msgs := make([]Message, n)
	for i := range msgs {
		msgs[i] = Message{Token: token(i)}
	}
	d.Enqueue(msgs)
}

// The point of the dispatcher over an inline send: a fan-out that produces many
// messages at once must become few requests, not many.
func TestDispatcherCoalescesIntoOneBatch(t *testing.T) {
	f := &fakeSender{}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{Workers: 1, FlushWindow: 50 * time.Millisecond})

	enqueueN(d, 25)
	d.Close()

	got := f.snapshot()
	if len(got) != 1 {
		t.Fatalf("made %d batches for 25 messages enqueued at once, want 1: %v", len(got), got)
	}
	if len(got[0]) != 25 {
		t.Fatalf("batch carried %d messages, want 25", len(got[0]))
	}
}

// The dispatcher must not hand the sender more than one request's worth, or the
// sender's own splitting is the only thing standing between us and a rejected
// request.
func TestDispatcherCapsBatchesAtMaxBatchSize(t *testing.T) {
	f := &fakeSender{}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{
		Workers:     1,
		QueueSize:   4 * MaxBatchSize,
		FlushWindow: 50 * time.Millisecond,
	})

	enqueueN(d, 2*MaxBatchSize+5)
	d.Close()

	for i, b := range f.snapshot() {
		if len(b) > MaxBatchSize {
			t.Fatalf("batch %d carried %d messages, over the %d cap", i, len(b), MaxBatchSize)
		}
	}
	if n := f.total(); n != 2*MaxBatchSize+5 {
		t.Fatalf("delivered %d messages, want %d", n, 2*MaxBatchSize+5)
	}
}

// Enqueue is called from inside a JetStream consumer callback, where blocking
// is what stalls event acking. A full queue must therefore drop, and Enqueue
// must return — this test hangs rather than fails if it ever blocks.
func TestDispatcherDropsRatherThanBlocksWhenFull(t *testing.T) {
	f := &fakeSender{block: make(chan struct{})}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{
		Workers:     1,
		QueueSize:   1,
		FlushWindow: time.Millisecond,
	})

	// The single worker wedges in Send; the one queue slot fills; the rest have
	// nowhere to go.
	enqueueN(d, 200)

	if got := d.Dropped(); got == 0 {
		t.Fatal("a queue of 1 behind a blocked sender must have dropped messages")
	}

	close(f.block)
	d.Close()
}

// Close is called after the event handlers have drained, so whatever is still
// queued is the push half of events this replica has already acked. Discarding
// it would silently lose them.
func TestCloseFlushesTheQueue(t *testing.T) {
	f := &fakeSender{block: make(chan struct{})}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{
		Workers:     1,
		QueueSize:   64,
		FlushWindow: time.Millisecond,
	})

	enqueueN(d, 32)

	// Let the worker through only once the whole batch is queued behind it.
	close(f.block)
	d.Close()

	if n := f.total(); n != 32 {
		t.Fatalf("delivered %d of 32 queued messages", n)
	}
}

func TestEnqueueAfterCloseIsANoOp(t *testing.T) {
	f := &fakeSender{}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{Workers: 1, FlushWindow: time.Millisecond})
	d.Close()

	// Must not panic (the queue channel is deliberately never closed) and must
	// not extend a completed drain.
	enqueueN(d, 10)
	d.Close() // idempotent

	if n := f.total(); n != 0 {
		t.Fatalf("delivered %d messages enqueued after Close", n)
	}
}

func TestEnqueueSkipsEmptyTokens(t *testing.T) {
	f := &fakeSender{}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{Workers: 1, FlushWindow: 20 * time.Millisecond})

	d.Enqueue([]Message{{Token: ""}, {Token: token(1)}, {Token: ""}})
	d.Close()

	if n := f.total(); n != 1 {
		t.Fatalf("delivered %d messages, want 1 (empty tokens must be skipped)", n)
	}
}

// A sender failure is logged, not retried: a retry would re-notify the devices
// whose messages in the same batch succeeded, and the notification row is
// already durable in Postgres.
func TestSenderFailureDoesNotWedgeTheDispatcher(t *testing.T) {
	f := &fakeSender{err: fmt.Errorf("expo is down")}
	d := NewDispatcher(f, quietLogger(), DispatcherConfig{Workers: 1, FlushWindow: time.Millisecond})

	enqueueN(d, 3)
	enqueueN(d, 3)
	d.Close()

	if n := f.total(); n != 6 {
		t.Fatalf("delivered %d messages after a failing send, want 6", n)
	}
}
