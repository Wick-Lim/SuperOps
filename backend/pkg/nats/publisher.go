package nats

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Publish is fire-and-forget core NATS: at-most-once, no ack, no persistence.
// Correct for ephemeral fan-out (typing indicators, presence, live relays)
// where a dropped message is invisible a second later. Anything a consumer
// must not miss belongs on PublishDurable.
func (c *Client) Publish(subject string, event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	if err := c.Conn.Publish(subject, data); err != nil {
		return fmt.Errorf("publish to %s: %w", subject, err)
	}

	return nil
}

// PublishDurable publishes through JetStream and waits for the server's
// storage ack, giving at-least-once delivery with redelivery on consumer
// failure. msgID, when non-empty, is sent as Nats-Msg-Id so the stream's
// duplicate window collapses retries of the same logical event — which is what
// makes "at least once" safe to retry into.
//
// The caller's ctx bounds the ack wait; the publish is not durable until this
// returns nil.
func (c *Client) PublishDurable(ctx context.Context, subject, msgID string, event Event) error {
	if c.JetStream == nil {
		return fmt.Errorf("publish to %s: JetStream is not configured", subject)
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	opts := []jetstream.PublishOpt{}
	if msgID != "" {
		opts = append(opts, jetstream.WithMsgID(msgID))
	}

	if _, err := c.JetStream.Publish(ctx, subject, data, opts...); err != nil {
		return fmt.Errorf("jetstream publish to %s: %w", subject, err)
	}

	return nil
}
