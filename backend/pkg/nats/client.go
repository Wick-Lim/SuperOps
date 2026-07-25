package nats

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// DefaultDrainTimeout bounds how long Close waits for subscription handlers to
// finish and for buffered publishes to flush.
const DefaultDrainTimeout = 10 * time.Second

type Config struct {
	URL string
	// DrainTimeout bounds Close/Drain. Zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
}

type Client struct {
	Conn         *nats.Conn
	JetStream    jetstream.JetStream
	logger       *slog.Logger
	drainTimeout time.Duration
}

func NewClient(cfg Config, logger *slog.Logger) (*Client, error) {
	drainTimeout := cfg.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeout
	}

	nc, err := nats.Connect(cfg.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.DrainTimeout(drainTimeout),
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
		Conn:         nc,
		JetStream:    js,
		logger:       logger,
		drainTimeout: drainTimeout,
	}, nil
}

// Drain stops accepting new messages, lets every subscription's handler finish
// the messages it has already been given, flushes pending publishes, and only
// then closes the connection. Conn.Close() on its own severs the socket
// immediately, so a SIGTERM used to discard whatever the indexer and notifier
// subscriptions were mid-way through.
//
// Drain blocks until the connection is closed or drainTimeout elapses; it is
// safe to call on an already-closed connection.
func (c *Client) Drain() error {
	if c.Conn == nil || c.Conn.IsClosed() {
		return nil
	}

	if err := c.Conn.Drain(); err != nil {
		// Drain failed to start (already draining/closed) — fall back to a hard
		// close so the caller never leaks the connection.
		c.Conn.Close()
		return fmt.Errorf("drain NATS connection: %w", err)
	}

	// Conn.Drain is asynchronous: it returns as soon as draining has *started*.
	deadline := time.Now().Add(c.drainTimeout + time.Second)
	for !c.Conn.IsClosed() {
		if time.Now().After(deadline) {
			c.Conn.Close()
			return fmt.Errorf("drain NATS connection: timed out after %s", c.drainTimeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

// Close drains first so in-flight messages are not dropped, then closes.
func (c *Client) Close() {
	if err := c.Drain(); err != nil && c.logger != nil {
		c.logger.Warn("NATS drain incomplete", "error", err)
	}
}
