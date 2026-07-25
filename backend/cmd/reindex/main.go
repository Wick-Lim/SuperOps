// Command reindex rebuilds the Meilisearch message index from Postgres.
//
// Nothing else in the tree can do this. The index is only ever written
// incrementally, by the worker's JetStream consumer reacting to live events, so
// after a Meilisearch data loss, a version upgrade that changes the schema, or
// a retention purge that predates search.Service.DeleteMessages, the index and
// the database drift apart with no way back.
//
// Usage:
//
//	reindex [-batch 500] [-workspace <uuid>] [-dry-run]
//
// It reads the same MEILI_HOST / MEILI_MASTER_KEY / SEARCH_ENABLED configuration
// as the server, walks messages in keyset order, and is safe to re-run: indexing
// a document that already exists is an upsert on the primary key.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

func main() { os.Exit(run()) }

func run() int {
	batch := flag.Int("batch", 500, "rows fetched per page")
	workspace := flag.String("workspace", "", "restrict the rebuild to one workspace id")
	dryRun := flag.Bool("dry-run", false, "count what would be indexed without writing to Meilisearch")
	flag.Parse()

	if *batch < 1 || *batch > 10000 {
		log.Print("-batch must be between 1 and 10000")
		return 2
	}

	cfg, err := app.LoadConfig()
	if err != nil {
		log.Print("load config: ", err)
		return 1
	}
	l := logger.New(cfg.LogLevel)

	if !cfg.Meili.IsEnabled() {
		l.Error("search is disabled (SEARCH_ENABLED=false or MEILI_HOST empty); nothing to reindex")
		return 1
	}

	// Ctrl-C stops after the current page instead of leaving a half-written
	// index with no indication of where it stopped.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.NewPool(ctx, database.Config{
		DSN:      cfg.DB.DSN(),
		MaxConns: cfg.DB.MaxConns,
		MinConns: cfg.DB.MinConns,
		// No statement timeout: a full-table walk is the point of this command.
	}, l)
	if err != nil {
		l.Error("database", "error", err)
		return 1
	}
	defer pool.Close()

	var svc *search.Service
	if !*dryRun {
		svc, err = search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l)
		if err != nil {
			l.Error("meilisearch", "error", err)
			return 1
		}
	}

	indexed, failed, err := reindex(ctx, pool, svc, *workspace, *batch, l)
	l.Info("reindex finished", "indexed", indexed, "failed", failed, "dry_run", *dryRun)
	if err != nil {
		l.Error("reindex stopped early", "error", err)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// pageSQL walks messages in (created_at, id) keyset order.
//
// Keyset rather than OFFSET: OFFSET re-scans every row it skips, so the last
// page of a large table costs as much as the whole walk. The tuple comparison
// (not created_at alone) is what makes the cursor total — messages promoted in
// the same scheduled tick used to share a timestamp exactly.
const pageSQL = `
	SELECT m.id, m.channel_id, c.workspace_id, m.user_id, m.content, m.created_at
	  FROM messages m
	  JOIN channels c ON c.id = m.channel_id
	 WHERE m.is_deleted = FALSE
	   AND m.is_scheduled = FALSE
	   AND ($1::uuid IS NULL OR c.workspace_id = $1::uuid)
	   AND (m.created_at, m.id) > ($2::timestamptz, $3::uuid)
	 ORDER BY m.created_at, m.id
	 LIMIT $4`

func reindex(
	ctx context.Context,
	pool *pgxpool.Pool,
	svc *search.Service,
	workspaceID string,
	batch int,
	l *slog.Logger,
) (indexed, failed int, err error) {
	var wsFilter *string
	if workspaceID != "" {
		wsFilter = &workspaceID
	}

	// Zero cursor: (epoch, all-zero uuid) sorts before every real row.
	cursorTime := time.Unix(0, 0).UTC()
	cursorID := "00000000-0000-0000-0000-000000000000"

	for {
		if err := ctx.Err(); err != nil {
			return indexed, failed, err
		}

		rows, qErr := pool.Query(ctx, pageSQL, wsFilter, cursorTime, cursorID, batch)
		if qErr != nil {
			return indexed, failed, fmt.Errorf("read messages: %w", qErr)
		}

		page := 0
		var docs []search.MessageDoc
		for rows.Next() {
			var (
				doc       search.MessageDoc
				createdAt time.Time
			)
			if scanErr := rows.Scan(&doc.ID, &doc.ChannelID, &doc.WorkspaceID, &doc.UserID, &doc.Content, &createdAt); scanErr != nil {
				rows.Close()
				return indexed, failed, fmt.Errorf("scan message: %w", scanErr)
			}
			doc.CreatedAt = createdAt.Unix()
			docs = append(docs, doc)
			cursorTime, cursorID = createdAt, doc.ID
			page++
		}
		rows.Close()
		if rowsErr := rows.Err(); rowsErr != nil {
			return indexed, failed, fmt.Errorf("read messages: %w", rowsErr)
		}
		if page == 0 {
			return indexed, failed, nil
		}

		for _, doc := range docs {
			if svc == nil { // dry run
				indexed++
				continue
			}
			if idxErr := svc.IndexMessage(doc); idxErr != nil {
				failed++
				l.Error("index message", "id", doc.ID, "error", idxErr)
				continue
			}
			indexed++
		}

		l.Info("reindex progress", "indexed", indexed, "failed", failed, "cursor", cursorTime.Format(time.RFC3339Nano))

		if page < batch {
			return indexed, failed, nil
		}
	}
}
