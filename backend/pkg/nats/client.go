package nats

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type Config struct {
	URL string
}

type Client struct {
	Conn      *nats.Conn
	JetStream jetstream.JetStream
	logger    *slog.Logger
}

func NewClient(cfg Config, logger *slog.Logger) (*Client, error) {
	nc, err := nats.Connect(cfg.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logger.Warn("NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			logger.Info("NATS reconnected")
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create JetStream context: %w", err)
	}

	logger.Info("connected to NATS", "url", cfg.URL)

	return &Client{
		Conn:      nc,
		JetStream: js,
		logger:    logger,
	}, nil
}

func (c *Client) Close() {
	c.Conn.Close()
}
