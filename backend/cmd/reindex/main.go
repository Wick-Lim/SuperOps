// Command reindex rebuilds the Meilisearch object index from Postgres.
//
// Nothing else in the tree can do this. The index is only ever written
// incrementally, by the worker's JetStream consumer reacting to live events, so
// after a Meilisearch data loss, a version upgrade that changes the schema, or
// a retention purge that predates search.Service.DeleteMessages, the index and
// the database drift apart with no way back.
//
// Usage:
//
//	reindex [-type all|message|file] [-batch 500] [-workspace <uuid>] [-dry-run]
//	reindex -prune-legacy
//
// It reads the same MEILI_HOST / MEILI_MASTER_KEY / SEARCH_ENABLED configuration
// as the server, walks each object type in keyset order, and is safe to re-run:
// indexing a document that already exists is an upsert on the primary key.
//
// # Migrating a deployment that predates typed objects
//
// The old index (`messages`, primary key `id`) cannot be converted in place —
// Meilisearch will not change the primary key of a non-empty index — so the new
// shape lives in `objects` and the old index is left alone until it is dropped
// explicitly:
//
//	# 1. before deploying the new API/worker: fill the new index. Nothing reads
//	#    it yet, so this is invisible to users and interruptible.
//	reindex
//	# 2. deploy the new API and worker.
//	# 3. backfill the gap: anything written between (1) and (2) landed in the
//	#    old index only. Upserts, so this is cheap.
//	reindex
//	# 4. when search looks right, reclaim the disk. Opt-in and separate, so a
//	#    rollback in (2) or (3) still has an index to roll back to.
//	reindex -prune-legacy
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
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
	typeName := flag.String("type", "all", "object type to rebuild: all, "+strings.Join(sourceNames(), ", "))
	dryRun := flag.Bool("dry-run", false, "count what would be indexed without writing to Meilisearch")
	pruneLegacy := flag.Bool("prune-legacy", false, "delete the pre-generalization `messages` index and exit")
	flag.Parse()

	if *batch < 1 || *batch > 10000 {
		log.Print("-batch must be between 1 and 10000")
		return 2
	}

	selected, err := selectSources(*typeName)
	if err != nil {
		log.Print(err)
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

	if *pruneLegacy {
		return prune(ctx, cfg, l, *dryRun)
	}

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

	var totalIndexed, totalFailed int
	for _, src := range selected {
		indexed, failed, err := rebuild(ctx, pool, svc, src, *workspace, *batch, l)
		totalIndexed += indexed
		totalFailed += failed
		l.Info("type finished", "type", src.typ, "indexed", indexed, "failed", failed, "dry_run", *dryRun)
		if err != nil {
			l.Error("reindex stopped early", "type", src.typ, "error", err)
			return 1
		}
	}

	l.Info("reindex finished", "indexed", totalIndexed, "failed", totalFailed, "dry_run", *dryRun)
	if totalFailed > 0 {
		return 1
	}
	return 0
}

// prune drops the legacy message-only index. It is a separate mode rather than
// a step of the rebuild: dropping it is irreversible, and the operator should
// be the one to decide that the new index is serving correctly.
func prune(ctx context.Context, cfg *app.Config, l *slog.Logger, dryRun bool) int {
	if dryRun {
		l.Info("dry run: would delete the legacy messages index")
		return 0
	}
	svc, err := search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l)
	if err != nil {
		l.Error("meilisearch", "error", err)
		return 1
	}
	if err := svc.DropLegacyIndex(ctx); err != nil {
		l.Error("prune legacy index", "error", err)
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// cursor is a keyset position: (created_at, id) of the last row of the previous
// page. The tuple, not created_at alone, is what makes the cursor total —
// messages promoted in the same scheduled tick share a timestamp exactly.
type cursor struct {
	at time.Time
	id string
}

// zeroCursor sorts before every real row.
func zeroCursor() cursor {
	return cursor{at: time.Unix(0, 0).UTC(), id: "00000000-0000-0000-0000-000000000000"}
}

// source is one object type's walk over Postgres. Adding a type to search is a
// doc constructor (internal/search) plus one entry here — not a new command and
// not a new index.
type source struct {
	typ search.ObjectType
	// sql selects one page after the cursor, with the same four parameters in
	// the same order: workspace filter, cursor timestamp, cursor id, limit.
	sql string
	// scan reads one row into a Doc and reports its cursor position.
	scan func(rows pgxRow) (search.Doc, cursor, error)
}

// pgxRow is the row-scanning surface of pgx.Rows, so a scan function can be
// tested without a database.
type pgxRow interface {
	Scan(dest ...any) error
}

// messageSQL walks messages in (created_at, id) keyset order.
//
// Keyset rather than OFFSET: OFFSET re-scans every row it skips, so the last
// page of a large table costs as much as the whole walk.
const messageSQL = `
	SELECT m.id, m.channel_id, c.workspace_id, m.user_id, m.content, m.created_at
	  FROM messages m
	  JOIN channels c ON c.id = m.channel_id
	 WHERE m.is_deleted = FALSE
	   AND m.is_scheduled = FALSE
	   AND ($1::uuid IS NULL OR c.workspace_id = $1::uuid)
	   AND (m.created_at, m.id) > ($2::timestamptz, $3::uuid)
	 ORDER BY m.created_at, m.id
	 LIMIT $4`

// fileSQL walks files, carrying the MATERIALIZED ACCESS KEYS.
//
// It read only (id, workspace, user, name, channel) until now, which made
// FileDoc.Doc() fall through to its pre-Drive derivation — "the channel key,
// else the uploader key". That derivation is exactly file.Handler.canRead and
// exactly right for a chat attachment, and WRONG for a Drive file, which is
// readable through its folder, through the workspace grant on the Drive root,
// or through a per-object grant.
//
// So running this tool NARROWED every Drive file in the index to its uploader
// alone. Reindex is the recovery path — it is what an operator runs when the
// index is wrong — and it silently un-shared the corpus instead.
//
// A SUBQUERY, not a join. A join against acl_key multiplies a row by its key
// count, and this query's (created_at, id) keyset cursor is built from the last
// row of a page: duplicated rows make a page hold fewer distinct files than it
// claims, and the cursor then skips the difference. That is the same reason
// internal/drive and internal/issue filter with EXISTS rather than joining.
//
// The messages join is still not filtered by messages.is_deleted: a
// soft-deleted message keeps its channel, and the materialized keys already say
// so.
//
// THE BODY IS JOINED IN FOR THE SAME REASON THE KEYS ARE. Both index writes are
// add-or-REPLACE, so a rebuild that selected no body wrote an empty `content`
// over every document, spreadsheet and design in the index — and only a fresh
// client projection would ever put it back. That is the same shape as the
// acl_key bug this query already carries the fix for: the recovery path
// destroying the thing it exists to restore. The live indexer reads the body
// through search.BodySource (drive.Repository.BodyForFile); this is the same
// read, expressed as a join because there is no per-row lookup here.
const fileSQL = `
	SELECT f.id, f.workspace_id, f.user_id, f.name,
	       COALESCE(m.channel_id::text, ''),
	       COALESCE(f.folder_id::text, ''),
	       f.file_type,
	       COALESCE(ARRAY(SELECT k.key FROM acl_key k
	                       WHERE k.object_type = 'file' AND k.object_id = f.id), '{}'),
	       COALESCE(p.body_text, ''),
	       f.created_at
	  FROM files f
	  LEFT JOIN messages m ON m.id = f.message_id
	  LEFT JOIN file_projections p ON p.file_id = f.id
	 WHERE ($1::uuid IS NULL OR f.workspace_id = $1::uuid)
	   -- TRASHED FILES ARE NOT SEARCHABLE, and this rebuild is the documented
	   -- recovery path — so without the predicate it UNDOES the unindexing that
	   -- trashing does, and every document anybody ever threw away comes back
	   -- into search the first time an operator runs it. There were 54 such
	   -- rows in a development database of 1039.
	   --
	   -- Removal is still the delete arm's job; this is only about what a full
	   -- rebuild writes. A trashed file that is somehow still in the index stays
	   -- there until it is purged, which publishes file.deleted.
	   AND f.trashed_at IS NULL
	   AND (f.created_at, f.id) > ($2::timestamptz, $3::uuid)
	 ORDER BY f.created_at, f.id
	 LIMIT $4`

func sources() []source {
	return []source{
		{
			typ: search.TypeMessage,
			sql: messageSQL,
			scan: func(row pgxRow) (search.Doc, cursor, error) {
				var (
					doc       search.MessageDoc
					createdAt time.Time
				)
				if err := row.Scan(&doc.ID, &doc.ChannelID, &doc.WorkspaceID, &doc.UserID, &doc.Content, &createdAt); err != nil {
					return search.Doc{}, cursor{}, fmt.Errorf("scan message: %w", err)
				}
				doc.CreatedAt = createdAt.Unix()
				return doc.Doc(), cursor{at: createdAt, id: doc.ID}, nil
			},
		},
		{
			typ: search.TypeFile,
			sql: fileSQL,
			scan: func(row pgxRow) (search.Doc, cursor, error) {
				var (
					doc       search.FileDoc
					createdAt time.Time
				)
				var fileType string
				if err := row.Scan(&doc.ID, &doc.WorkspaceID, &doc.UserID, &doc.Name,
					&doc.ChannelID, &doc.FolderID, &fileType, &doc.ACL, &doc.Body,
					&createdAt); err != nil {
					return search.Doc{}, cursor{}, fmt.Errorf("scan file: %w", err)
				}
				// The same mapper the live indexer uses. Without it a rebuild
				// writes every document back under file_<uuid> — the twin the
				// indexer's transitional sweep exists to remove — and ?type=document
				// returns nothing the day after a recovery.
				doc.Type = search.FileObjectType(fileType)
				doc.CreatedAt = createdAt.Unix()
				return doc.Doc(), cursor{at: createdAt, id: doc.ID}, nil
			},
		},
	}
}

func sourceNames() []string {
	names := make([]string, 0, len(sources()))
	for _, s := range sources() {
		names = append(names, string(s.typ))
	}
	return names
}

// selectSources resolves the -type flag. "all" means every type that has a
// source today, not every type search.ObjectTypes declares: a type with no
// producer would rebuild to nothing and report success.
func selectSources(name string) ([]source, error) {
	all := sources()
	if name == "" || name == "all" {
		return all, nil
	}
	for _, s := range all {
		if string(s.typ) == name {
			return []source{s}, nil
		}
	}
	return nil, fmt.Errorf("-type must be all or one of %s", strings.Join(sourceNames(), ", "))
}

// ---------------------------------------------------------------------------
// The walk
// ---------------------------------------------------------------------------

func rebuild(
	ctx context.Context,
	pool *pgxpool.Pool,
	svc *search.Service,
	src source,
	workspaceID string,
	batch int,
	l *slog.Logger,
) (indexed, failed int, err error) {
	var wsFilter *string
	if workspaceID != "" {
		wsFilter = &workspaceID
	}

	cur := zeroCursor()

	for {
		if err := ctx.Err(); err != nil {
			return indexed, failed, err
		}

		rows, qErr := pool.Query(ctx, src.sql, wsFilter, cur.at, cur.id, batch)
		if qErr != nil {
			return indexed, failed, fmt.Errorf("read %s rows: %w", src.typ, qErr)
		}

		page := 0
		var docs []search.Doc
		for rows.Next() {
			doc, next, scanErr := src.scan(rows)
			if scanErr != nil {
				rows.Close()
				return indexed, failed, scanErr
			}
			docs = append(docs, doc)
			cur = next
			page++
		}
		rows.Close()
		if rowsErr := rows.Err(); rowsErr != nil {
			return indexed, failed, fmt.Errorf("read %s rows: %w", src.typ, rowsErr)
		}
		if page == 0 {
			return indexed, failed, nil
		}

		if svc == nil { // dry run
			indexed += page
		} else {
			ok, bad := writePage(ctx, svc, docs, src.typ, l)
			indexed += ok
			failed += bad
		}

		l.Info("reindex progress", "type", src.typ, "indexed", indexed, "failed", failed,
			"cursor", cur.at.Format(time.RFC3339Nano))

		if page < batch {
			return indexed, failed, nil
		}
	}
}

// writePage indexes one page, falling back to one document at a time when the
// batch is rejected.
//
// The batch is a single HTTP request for up to `batch` documents — the previous
// version made one request per document, which is what made a full rebuild an
// overnight job. But a batch is all-or-nothing: one row that cannot be turned
// into a valid document (no workspace, no channel, so no access keys) would
// take the other 499 with it. The fallback isolates the bad rows, so a rebuild
// reports exactly which objects it could not index and indexes everything else.
func writePage(ctx context.Context, svc *search.Service, docs []search.Doc, typ search.ObjectType, l *slog.Logger) (indexed, failed int) {
	if err := svc.IndexBatch(ctx, docs); err == nil {
		return len(docs), 0
	} else if !permanent(err) {
		// Meilisearch is unreachable or refusing work: retrying the same page
		// document by document would just make N failures out of one.
		l.Error("index batch", "type", typ, "count", len(docs), "error", err)
		return 0, len(docs)
	}

	for _, doc := range docs {
		if err := svc.IndexAwait(ctx, doc); err != nil {
			failed++
			l.Error("index document", "type", typ, "id", doc.ID, "error", err)
			continue
		}
		indexed++
	}
	return indexed, failed
}

// permanent reports whether an error is one redelivery cannot fix. It matches
// structurally, the same way cmd/worker does, rather than importing the
// concrete type.
func permanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}
