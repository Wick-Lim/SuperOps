package message

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// OutboxEvent represents one durable domain event row in domain_event_outbox
// (migration 065). Every message event is persisted here before or alongside
// the direct JetStream publish; if NATS is unreachable or the process crashes
// between the PostgreSQL commit and the JetStream ack, the worker's outbox
// sweep replays the row using the same DedupeID.
type OutboxEvent struct {
	ID        int64           `json:"id"`
	Subject   string          `json:"subject"`
	DedupeID  string          `json:"dedupe_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
	Attempts  int             `json:"attempts"`
}

// RecordOutboxEvent inserts or returns an existing outbox entry keyed on dedupeID.
func (r *Repository) RecordOutboxEvent(ctx context.Context, subject, dedupeID, eventType string, data any) (int64, error) {
	if r == nil || r.pool == nil || subject == "" || dedupeID == "" {
		return 0, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshal outbox payload: %w", err)
	}
	var id int64
	err = r.pool.QueryRow(ctx,
		`INSERT INTO domain_event_outbox (subject, dedupe_id, event_type, payload)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (dedupe_id) DO UPDATE SET subject = EXCLUDED.subject
		 RETURNING id`,
		subject, dedupeID, eventType, raw,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert outbox event: %w", err)
	}
	return id, nil
}

// MarkOutboxPublished marks an outbox row as acknowledged by JetStream.
func (r *Repository) MarkOutboxPublished(ctx context.Context, id int64) error {
	if r == nil || r.pool == nil || id <= 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE domain_event_outbox
		    SET published_at = NOW(),
		        last_error = NULL
		  WHERE id = $1 AND published_at IS NULL`,
		id,
	)
	return err
}

// RecordOutboxFailure records a transient publish error and schedules retry backoff.
func (r *Repository) RecordOutboxFailure(ctx context.Context, id int64, publishErr error) error {
	if r == nil || r.pool == nil || id <= 0 || publishErr == nil {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE domain_event_outbox
		    SET attempts = attempts + 1,
		        last_error = $2,
		        next_attempt_at = NOW() + (LEAST(attempts + 1, 6) * INTERVAL '5 seconds')
		  WHERE id = $1 AND published_at IS NULL`,
		id, publishErr.Error(),
	)
	return err
}

// SweepOutbox claims due unpublished events and hands each to publishFn,
// marking rows published when publishFn succeeds and backing off when it fails.
func (r *Repository) SweepOutbox(ctx context.Context, limit int, publishFn func(ctx context.Context, ev OutboxEvent) error) (int, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, subject, dedupe_id, event_type, payload, attempts
		   FROM domain_event_outbox
		  WHERE published_at IS NULL
		    AND next_attempt_at <= NOW()
		  ORDER BY id ASC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("query outbox: %w", err)
	}
	var pending []OutboxEvent
	for rows.Next() {
		var ev OutboxEvent
		if err := rows.Scan(&ev.ID, &ev.Subject, &ev.DedupeID, &ev.EventType, &ev.Payload, &ev.Attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		pending = append(pending, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	published := 0
	for _, ev := range pending {
		pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := publishFn(pubCtx, ev)
		cancel()
		if err != nil {
			_ = r.RecordOutboxFailure(ctx, ev.ID, err)
			continue
		}
		if err := r.MarkOutboxPublished(ctx, ev.ID); err == nil {
			published++
		}
	}
	return published, nil
}
