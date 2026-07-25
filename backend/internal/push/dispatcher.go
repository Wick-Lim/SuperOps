package push

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Dispatcher decouples "a notification was created" from "an HTTPS request to
// Expo completed".
//
// The alternative — sending inline from the notification fan-out — is what
// makes this worth its complexity. The fan-out runs inside a durable JetStream
// consumer callback with a 25s handler budget and MaxAckPending of 256, so an
// inline POST puts a third party on the public internet directly in the path of
// event acking: Expo being slow would stall the consumer, and Expo being down
// would stall it for the full timeout on every event. It would also defeat
// batching outright, since the fan-out produces one message per recipient
// device and would send each one on its own.
//
// So Enqueue is non-blocking and lossy by design. Losing a push is survivable —
// the notification row is already committed and the recipient sees it on their
// next fetch — whereas blocking the consumer is not.
type Dispatcher struct {
	queue  chan Message
	sender Sender
	logger *slog.Logger

	// flushWindow is how long a worker waits for a batch to fill before sending
	// what it has. It trades a few milliseconds of delivery latency for far
	// fewer requests when one message fans out to many recipients.
	flushWindow time.Duration
	// sendTimeout bounds one Send from the dispatcher's own context, which is
	// not the request's — by the time a batch is sent the event that produced
	// it has usually been acked.
	sendTimeout time.Duration

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	dropped atomic.Int64
}

// DispatcherConfig tunes the queue. Every field has a usable zero value.
type DispatcherConfig struct {
	// QueueSize bounds the backlog. Zero means DefaultQueueSize.
	QueueSize int
	// Workers is the number of concurrent senders. Zero means DefaultWorkers.
	Workers int
	// FlushWindow is how long to wait for a batch to fill. Zero means
	// DefaultFlushWindow.
	FlushWindow time.Duration
	// SendTimeout bounds one Send. Zero means DefaultSendTimeout.
	SendTimeout time.Duration
}

// Dispatcher defaults. The queue is deliberately generous relative to the
// worker count: a burst is normal (one message to a busy channel is one push
// per member device), a sustained overflow is not.
const (
	DefaultQueueSize   = 4096
	DefaultWorkers     = 2
	DefaultFlushWindow = 100 * time.Millisecond
	DefaultSendTimeout = 30 * time.Second
)

// NewDispatcher starts the worker pool. Call Close to drain it.
func NewDispatcher(sender Sender, logger *slog.Logger, cfg DispatcherConfig) *Dispatcher {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.FlushWindow <= 0 {
		cfg.FlushWindow = DefaultFlushWindow
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = DefaultSendTimeout
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	d := &Dispatcher{
		queue:       make(chan Message, cfg.QueueSize),
		sender:      sender,
		logger:      logger,
		flushWindow: cfg.FlushWindow,
		sendTimeout: cfg.SendTimeout,
		done:        make(chan struct{}),
	}
	d.wg.Add(cfg.Workers)
	for range cfg.Workers {
		go d.run()
	}
	return d
}

// Enqueue queues msgs for delivery and returns immediately.
//
// It never blocks and never panics: the queue channel is never closed (Close
// signals through a separate channel precisely so that a concurrent Enqueue
// cannot send on a closed channel), and a full queue drops rather than waits.
func (d *Dispatcher) Enqueue(msgs []Message) {
	select {
	case <-d.done:
		// Shutting down. Accepting more would extend the drain indefinitely.
		return
	default:
	}

	for _, m := range msgs {
		if m.Token == "" {
			continue
		}
		select {
		case d.queue <- m:
		default:
			if n := d.dropped.Add(1); n == 1 || n%100 == 0 {
				d.logger.Warn("push: queue full, dropping notification",
					"dropped_total", n, "queue_size", cap(d.queue))
			}
		}
	}
}

// Dropped reports how many messages the queue has discarded. Exposed so the
// worker can surface a persistent overflow rather than only logging it.
func (d *Dispatcher) Dropped() int64 { return d.dropped.Load() }

// Close stops accepting work, flushes what is already queued and waits for the
// workers. It is safe to call more than once.
func (d *Dispatcher) Close() {
	d.closeOnce.Do(func() { close(d.done) })
	d.wg.Wait()
}

func (d *Dispatcher) run() {
	defer d.wg.Done()
	for {
		select {
		case first := <-d.queue:
			d.send(d.collect(first))
		case <-d.done:
			d.drain()
			return
		}
	}
}

// collect accumulates up to MaxBatchSize messages, waiting at most flushWindow.
//
// It deliberately does not abandon the window when Close is signalled. Cutting
// it short would split one fan-out across two requests purely because shutdown
// happened to land in the middle of it, and the only thing gained is at most
// flushWindow of shutdown latency — which drain() would have spent anyway.
func (d *Dispatcher) collect(first Message) []Message {
	batch := make([]Message, 0, MaxBatchSize)
	batch = append(batch, first)

	timer := time.NewTimer(d.flushWindow)
	defer timer.Stop()

	for len(batch) < MaxBatchSize {
		select {
		case m := <-d.queue:
			batch = append(batch, m)
		case <-timer.C:
			return batch
		}
	}
	return batch
}

// drain flushes whatever is still queued at shutdown, without waiting for more.
func (d *Dispatcher) drain() {
	batch := make([]Message, 0, MaxBatchSize)
	for {
		select {
		case m := <-d.queue:
			batch = append(batch, m)
			if len(batch) == MaxBatchSize {
				d.send(batch)
				batch = batch[:0]
			}
		default:
			d.send(batch)
			return
		}
	}
}

func (d *Dispatcher) send(batch []Message) {
	if len(batch) == 0 {
		return
	}
	// context.Background, not a cancelled shutdown context: this is the last
	// chance these messages get, and the timeout is what bounds it.
	ctx, cancel := context.WithTimeout(context.Background(), d.sendTimeout)
	defer cancel()

	if err := d.sender.Send(ctx, batch); err != nil {
		// Logged, not retried. A retry would re-notify the devices whose
		// messages in the same batch succeeded, and the durable record of the
		// notification is already in Postgres.
		d.logger.Warn("push: batch send failed", "count", len(batch), "error", err)
	}
}
