package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	l := logger.New(cfg.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	application, err := app.New(ctx, cfg, l)
	if err != nil {
		l.Error("failed to initialize application", "error", err)
		os.Exit(1)
	}
	defer application.Close()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		l.Info("starting server", "addr", application.Server.Addr)
		if err := application.Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			l.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	l.Info("shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := application.Server.Shutdown(shutdownCtx); err != nil {
		l.Error("server forced to shutdown", "error", err)
	}

	l.Info("server stopped")
}
