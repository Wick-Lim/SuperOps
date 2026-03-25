package nats

import (
	"encoding/json"
	"fmt"
)

type Event struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

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
