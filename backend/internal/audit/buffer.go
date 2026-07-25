package audit

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// BufferConfig tunes the Tier 2 queue.
type BufferConfig struct {
	// Size is the queue depth. Past it, entries are DROPPED — counted, logged
	// and visible on /metrics, never silently.
	Size int
	// Workers drain the queue. Batching is by workspace inside writeBatch, so
	// more workers means more concurrent chain-head locks on DIFFERENT
	// workspaces and no more contention on the same one.
	Workers int
	// FlushInterval bounds how long an entry waits behind a partially-full
	// batch. A record that is written eventually is fine; a record that waits
	// for a batch that never fills is not.
	FlushInterval time.Duration
	// BatchSize bounds one transaction.
	BatchSize int
	// WriteTimeout bounds one batch write. It is detached from any request
	// context by construction — the caller's request finished long ago.
	WriteTimeout time.Duration
}

// DefaultBufferConfig is deliberately small. Tier 2 is egress and sensitive
// reads, which coalesce; a deep queue would mostly buy the ability to lose more
// at once.
var DefaultBufferConfig = BufferConfig{
	Size:          4096,
	Workers:       2,
	FlushInterval: 2 * time.Second,
	BatchSize:     64,
	WriteTimeout:  10 * time.Second,
}

// buffer is the bounded queue behind Service.Buffer.
//
// Its shape is push.Dispatcher's, including the Dropped() counter and the
// drain-before-exit ordering, because that shape is already load-bearing in this
// tree and a second one would be a second thing to reason about.
type buffer struct {
	ch      chan Entry
	dropped atomic.Int64
	wg      sync.WaitGroup
	once    sync.Once
	cfg     BufferConfig
	logger  *slog.Logger
	write   func(context.Context, []Entry) error
}

// StartBuffer turns on the Tier 2 queue. Calling it twice is a no-op; a Service
// without it falls back to synchronous writes, which is correct and slower.
func (s *Service) StartBuffer(cfg BufferConfig) {
	if s.buffer != nil {
		return
	}
	if cfg.Size <= 0 {
		cfg.Size = DefaultBufferConfig.Size
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultBufferConfig.Workers
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultBufferConfig.FlushInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBufferConfig.BatchSize
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultBufferConfig.WriteTimeout
	}

	b := &buffer{
		ch:     make(chan Entry, cfg.Size),
		cfg:    cfg,
		logger: s.logger,
		write: func(ctx context.Context, batch []Entry) error {
			if err := s.writeBatch(ctx, batch); err != nil {
				s.failures.Add(int64(len(batch)))
				return err
			}
			return nil
		},
	}
	s.buffer = b

	for range cfg.Workers {
		b.wg.Add(1)
		go b.run()
	}
}

// Dropped is the count of Tier 2 entries the queue refused since boot.
//
// This number MUST be on /metrics and MUST be alertable. A full buffer drops
// exactly when the load is interesting — during an incident — and silently
// dropping audit records is the failure that makes the entire surface
// worthless. It is a counted, logged drop precisely so that it is not silent.
func (s *Service) Dropped() int64 {
	if s.buffer == nil {
		return 0
	}
	return s.buffer.dropped.Load()
}

// Close drains the queue and stops the workers. cmd/worker and the API both call
// it during shutdown, AFTER everything that could still enqueue has stopped —
// the same ordering push.Dispatcher requires, for the same reason: closing first
// would discard the records for the last requests this replica served.
func (s *Service) Close() {
	if s.buffer == nil {
		return
	}
	s.buffer.close()
	if n := s.buffer.dropped.Load(); n > 0 {
		s.logger.Warn("audit: entries dropped by a full buffer during this run", "count", n)
	}
}

func (b *buffer) enqueue(e Entry) {
	select {
	case b.ch <- e:
	default:
		n := b.dropped.Add(1)
		// Logged on the first drop and then on a widening scale: an incident
		// that drops ten thousand records must not also produce ten thousand log
		// lines competing with the incident's own.
		if n == 1 || n%1000 == 0 {
			b.logger.Error("audit: buffer full, entry dropped",
				"dropped_total", n, "action", e.Action, "actor_id", e.ActorID)
		}
	}
}

func (b *buffer) run() {
	defer b.wg.Done()

	batch := make([]Entry, 0, b.cfg.BatchSize)
	ticker := time.NewTicker(b.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), b.cfg.WriteTimeout)
		if err := b.write(ctx, batch); err != nil {
			b.logger.Error("audit: buffered batch write failed", "error", err, "entries", len(batch))
		}
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-b.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= b.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (b *buffer) close() {
	b.once.Do(func() { close(b.ch) })
	b.wg.Wait()
}
