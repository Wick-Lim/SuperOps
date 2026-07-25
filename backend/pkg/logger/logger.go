package logger

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey struct{}

func New(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: lvl == slog.LevelDebug,
	})

	l := slog.New(handler)

	// Install as the process default. Without this, every slog.Default() call
	// — including FromContext's fallback and any library that logs through
	// slog — writes plain text to stderr at Info level, bypassing this
	// handler's format and level entirely.
	slog.SetDefault(l)

	return l
}

// FromContext returns the request-scoped logger stored by WithContext, or the
// process default (configured by New) when the context carries none.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}
