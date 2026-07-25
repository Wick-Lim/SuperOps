package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

// shutdownTimeout bounds the whole drain: in-flight HTTP requests first, then
// the WebSocket close handshakes.
const shutdownTimeout = 30 * time.Second

func main() {
	// run returns an exit code instead of calling os.Exit itself, so every
	// deferred cleanup in it actually runs. The old code called os.Exit(1) from
	// inside the ListenAndServe goroutine, which skipped `defer app.Close()`
	// entirely and left the pool, the Redis client and the NATS connection to be
	// reaped by process death instead of closed.
	os.Exit(run())
}

func run() int {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Print("load config: ", err)
		return 1
	}

	l := logger.New(cfg.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	application, err := app.New(ctx, cfg, l)
	if err != nil {
		l.Error("failed to initialize application", "error", err)
		return 1
	}
	defer application.Close()

	quit := make(chan os.Signal, 2)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	// serveErr carries a listener failure back to the main goroutine so it
	// unwinds through the same shutdown path a signal takes.
	serveErr := make(chan error, 1)
	go func() {
		l.Info("starting server", "addr", application.Server.Addr)
		err := application.Server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	exitCode := 0
	select {
	case err := <-serveErr:
		if err != nil {
			l.Error("server error", "error", err)
			exitCode = 1
		}
	case sig := <-quit:
		l.Info("shutting down server...", "signal", sig.String())
	}

	// Fail readiness immediately so the load balancer stops routing here while
	// the requests already in flight finish.
	application.BeginDrain()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// A second signal during the drain means "stop waiting". Previously it was
	// ignored, so an operator who wanted out had to reach for SIGKILL.
	go func() {
		select {
		case sig := <-quit:
			l.Warn("second signal during shutdown, forcing exit", "signal", sig.String())
			shutdownCancel()
		case <-shutdownCtx.Done():
		}
	}()

	if err := application.Server.Shutdown(shutdownCtx); err != nil {
		l.Error("server forced to shutdown", "error", err)
	}

	// http.Server.Shutdown explicitly does not wait for hijacked connections, so
	// without this every WebSocket dies at process exit with no close frame and
	// clients reconnect off a TCP reset instead of a clean 1001 going-away.
	// It must run before Close(): the read pumps still query the pool.
	application.Hub.Shutdown()

	l.Info("server stopped")
	return exitCode
}
