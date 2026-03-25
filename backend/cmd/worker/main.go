package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	l := logger.New(cfg.LogLevel)
	l.Info("starting worker")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = ctx
	_ = cfg

	// TODO: Initialize NATS subscriptions for:
	// - Search indexing (message.created -> Meilisearch)
	// - Notification dispatch (message.created -> email/push)
	// - File processing (file.uploaded -> thumbnail generation)
	// - Session cleanup (periodic expired session removal)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	l.Info("worker ready, waiting for events...")
	<-quit

	l.Info("worker shutting down")
}
