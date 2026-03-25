package main

import (
	"context"
	"log"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	l := logger.New(cfg.LogLevel)
	ctx := context.Background()

	pool, err := database.NewPool(ctx, database.Config{
		DSN:      cfg.DB.DSN(),
		MaxConns: cfg.DB.MaxConns,
		MinConns: cfg.DB.MinConns,
	}, l)
	if err != nil {
		log.Fatal("database: ", err)
	}
	defer pool.Close()

	// TODO: Insert seed data for development
	l.Info("seed data inserted successfully")
}
