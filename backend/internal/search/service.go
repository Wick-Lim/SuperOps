// Package search indexes every searchable object in a workspace — messages,
// files, and the document/spreadsheet/design/issue types the roadmap adds — in
// one Meilisearch index, and answers one query across all of them.
//
// # Authorization
//
// Search is a security boundary, not a convenience. Every stored object carries
// the set of access keys that grant read on it (see doc.go); every query is
// constrained to the keys the caller holds, derived from internal/authz. A
// caller with no keys gets an empty result, never an unconstrained query.
//
// # Migrating an existing deployment
//
// An installation from before this change has a populated `messages` index in
// the old shape: primary key `id`, one document per message, no `type` and no
// `acl`. Meilisearch cannot change the primary key of a non-empty index, so the
// new shape lives in a new index (`objects`) and the old one is left untouched
// until it is explicitly dropped. The rollout is:
//
//  1. Build the new binaries but do not deploy them yet. Run the new
//     `cmd/reindex` against the live database — it only writes `objects`, which
//     nothing reads yet, so the running API keeps serving from `messages`.
//  2. Deploy the API and the worker. From here writes go to `objects`.
//  3. Run `cmd/reindex` once more. Messages written between step 1 and step 2
//     went only to the old index; every write is an upsert on the primary key,
//     so the second pass is free apart from the walk.
//  4. Once search looks right, `cmd/reindex -prune-legacy` deletes the old
//     `messages` index. It is a separate, opt-in step so that a rollback in
//     step 2 or 3 still has an index to roll back to.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/meilisearch/meilisearch-go"
)

const (
	// objectsIndex holds every object type. One index rather than one per type:
	// a single BM25 ordering across types is the whole point of "one search",
	// and a `type` filter narrows just as well as a separate index would.
	objectsIndex = "objects"

	// legacyMessagesIndex is the pre-generalization message-only index. Nothing
	// in this build reads or writes it; DropLegacyIndex removes it when the
	// operator says so.
	legacyMessagesIndex = "messages"
)

const (
	// taskPollInterval / taskAwaitTimeout bound the wait for an accepted task to
	// actually be applied. The timeout stays well under the worker's 30s AckWait
	// so a slow Meilisearch produces a Nak-and-retry rather than a redelivery
	// racing an in-flight handler; every write here is an upsert or a delete on
	// the primary key, so retrying is free of side effects.
	taskPollInterval = 25 * time.Millisecond
	taskAwaitTimeout = 10 * time.Second

	// settingsTimeout bounds the whole startup reconciliation in NewService.
	settingsTimeout = 30 * time.Second

	// defaultLimit is the page size when a caller does not ask for one.
	defaultLimit = 20
)

// quoteFilter escapes a value for safe interpolation into a Meilisearch filter
// expression (defends against breaking out of the quoted string). Every value
// that reaches it is already canonicalised to a UUID or an access key, so this
// is the second of two layers rather than the only one.
func quoteFilter(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
}

// canonicalUUID re-renders an id in canonical lowercase hyphenated form, or
// reports false. The filter DSL is built by string concatenation, so only
// values proven to be [0-9a-f-] are ever allowed into it.
func canonicalUUID(s string) (string, bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

type Service struct {
	client meilisearch.ServiceManager
	logger *slog.Logger
}

// wantSearchable, wantFilterable and wantSortable are the index settings this
// build requires. wantFilterable in particular is load-bearing for
// authorization: buildFilter constrains every query by workspace_id and the
// caller's access keys, and Meilisearch rejects a filter on an attribute that
// is not filterable. Getting that set wrong therefore does not widen search
// results, it breaks search outright — which is exactly why the update must be
// verified rather than fired and forgotten.
//
// Searchable order is ranking order: a hit on a title outranks a hit in a body.
var (
	wantSearchable = []string{"title", "content"}
	wantFilterable = []string{"acl", "type", "channel_id", "workspace_id", "user_id", "created_at", "is_deleted"}
	wantSortable   = []string{"created_at"}
)

// hitAttributes is what a search asks Meilisearch to return. It is the Hit
// struct's fields and nothing else: `acl` must not leave the server, because an
// object's key set can name the users it was shared with.
var hitAttributes = []string{
	"id", "type", "workspace_id", "channel_id", "user_id", "title", "content", "created_at", "is_deleted",
}

func NewService(host, masterKey string, logger *slog.Logger) (*Service, error) {
	client := meilisearch.New(host, meilisearch.WithAPIKey(masterKey))

	ctx, cancel := context.WithTimeout(context.Background(), settingsTimeout)
	defer cancel()

	// Ensure index exists.
	if _, err := client.GetIndexWithContext(ctx, objectsIndex); err != nil {
		task, createErr := client.CreateIndexWithContext(ctx, &meilisearch.IndexConfig{
			Uid:        objectsIndex,
			PrimaryKey: "doc_id",
		})
		if createErr != nil {
			return nil, fmt.Errorf("create index: %w", createErr)
		}
		if err := awaitTask(ctx, client, task, "create index"); err != nil {
			// Losing the create race against another booting process is not a
			// failure: what matters is that the index is there now.
			if _, exists := client.GetIndexWithContext(ctx, objectsIndex); exists != nil {
				return nil, err
			}
		} else {
			logger.Info("created Meilisearch index", "index", objectsIndex)
		}
	}

	s := &Service{client: client, logger: logger}
	if err := s.ensureSettings(ctx); err != nil {
		return nil, err
	}

	logger.Info("connected to Meilisearch", "host", host, "index", objectsIndex)
	return s, nil
}

// ensureSettings reconciles the index settings with what this build needs.
//
// It reads before it writes. The previous version PUT all three settings on
// every process start, which (a) enqueued three settings tasks per boot — and
// a settings change makes Meilisearch re-index the whole collection, so a
// rolling restart of N replicas meant 3N full re-indexes — and (b) discarded
// the returned task, so a rejected filterable-attributes update looked exactly
// like an applied one while search silently 400'd on every query afterwards.
func (s *Service) ensureSettings(ctx context.Context) error {
	idx := s.client.Index(objectsIndex)

	// Searchable attributes are ordered: the position of an attribute is part of
	// the ranking rules, so this one compares as a sequence, not a set.
	current, err := idx.GetSearchableAttributesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("get searchable attributes: %w", err)
	}
	if !sameSequence(deref(current), wantSearchable) {
		// A copy: the client takes a pointer, and the package-level want* slices
		// are shared by every Service in the process.
		request := append([]string(nil), wantSearchable...)
		task, err := idx.UpdateSearchableAttributesWithContext(ctx, &request)
		if err != nil {
			return fmt.Errorf("update searchable attributes: %w", err)
		}
		if err := awaitTask(ctx, s.client, task, "update searchable attributes"); err != nil {
			return err
		}
		s.logger.Info("meilisearch settings updated", "setting", "searchable", "attributes", wantSearchable)
	}

	filterable, err := idx.GetFilterableAttributesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("get filterable attributes: %w", err)
	}
	if !sameSet(attributeNames(filterable), wantFilterable) {
		request := make([]interface{}, len(wantFilterable))
		for i, a := range wantFilterable {
			request[i] = a
		}
		task, err := idx.UpdateFilterableAttributesWithContext(ctx, &request)
		if err != nil {
			return fmt.Errorf("update filterable attributes: %w", err)
		}
		if err := awaitTask(ctx, s.client, task, "update filterable attributes"); err != nil {
			return err
		}
		s.logger.Info("meilisearch settings updated", "setting", "filterable", "attributes", wantFilterable)
	}

	sortable, err := idx.GetSortableAttributesWithContext(ctx)
	if err != nil {
		return fmt.Errorf("get sortable attributes: %w", err)
	}
	if !sameSet(deref(sortable), wantSortable) {
		request := append([]string(nil), wantSortable...)
		task, err := idx.UpdateSortableAttributesWithContext(ctx, &request)
		if err != nil {
			return fmt.Errorf("update sortable attributes: %w", err)
		}
		if err := awaitTask(ctx, s.client, task, "update sortable attributes"); err != nil {
			return err
		}
		s.logger.Info("meilisearch settings updated", "setting", "sortable", "attributes", wantSortable)
	}

	return nil
}

// awaitTask blocks until Meilisearch has actually applied an accepted task.
//
// Every write endpoint answers 202 with a task handle and nothing else, so a
// nil error from AddDocuments means "queued", not "indexed". Discarding the
// handle makes a rejected write indistinguishable from an applied one — and
// for the durable indexer that difference is the whole ack decision.
func awaitTask(ctx context.Context, client meilisearch.ServiceManager, info *meilisearch.TaskInfo, what string) error {
	if info == nil {
		return nil
	}
	task, err := client.WaitForTaskWithContext(ctx, info.TaskUID, taskPollInterval)
	if err != nil {
		return fmt.Errorf("%s: await task %d: %w", what, info.TaskUID, err)
	}
	switch task.Status {
	case meilisearch.TaskStatusSucceeded:
		return nil
	case meilisearch.TaskStatusFailed:
		// Meilisearch accepted the request and then rejected the work: the same
		// document will be rejected identically on every redelivery.
		return &PermanentError{
			Reason: fmt.Sprintf("%s: meilisearch rejected task %d", what, info.TaskUID),
			Err:    errors.New(task.Error.Message),
		}
	default:
		return fmt.Errorf("%s: task %d ended %s", what, info.TaskUID, task.Status)
	}
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// IndexAwait upserts one object and waits for Meilisearch to apply it. The wait
// is capped so a Meilisearch that has stopped draining its queue produces a
// retry rather than pinning the caller until its own deadline.
//
// A caller that has to decide whether the write really happened — the durable
// indexer, which acks a JetStream message on the strength of it — must use this
// rather than IndexBatch.
func (s *Service) IndexAwait(ctx context.Context, doc Doc) error {
	valid, err := doc.validate()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, taskAwaitTimeout)
	defer cancel()

	info, err := s.client.Index(objectsIndex).AddDocumentsWithContext(ctx, []Doc{valid}, nil)
	if err != nil {
		return fmt.Errorf("index %s %s: %w", valid.Type, valid.ID, err)
	}
	return awaitTask(ctx, s.client, info, "index "+string(valid.Type))
}

// IndexBatch enqueues a whole page of objects in one request and returns as
// soon as Meilisearch has accepted the task.
//
// Bulk callers (cmd/reindex) want this form: a round trip per document, waited
// on at ~50ms a piece, would take days for a million messages. Documents are
// validated first, and one bad document fails the batch rather than being
// dropped from it — a rebuild that silently skips rows is worse than one that
// stops and says which row.
func (s *Service) IndexBatch(ctx context.Context, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	valid := make([]Doc, 0, len(docs))
	for _, doc := range docs {
		v, err := doc.validate()
		if err != nil {
			return err
		}
		valid = append(valid, v)
	}

	ctx, cancel := context.WithTimeout(ctx, taskAwaitTimeout)
	defer cancel()

	if _, err := s.client.Index(objectsIndex).AddDocumentsWithContext(ctx, valid, nil); err != nil {
		return fmt.Errorf("index %d documents: %w", len(valid), err)
	}
	return nil
}

// DeleteAwait removes one object and waits for the deletion to be applied.
// Deleting an id that is not in the index is a success, so the durable indexer
// can safely be redelivered the same deletion.
func (s *Service) DeleteAwait(ctx context.Context, t ObjectType, objectID string) error {
	docID := DocID(t, objectID)
	if docID == "" {
		return &PermanentError{Reason: fmt.Sprintf("cannot unindex %s %q: not a known type and uuid", t, objectID)}
	}

	ctx, cancel := context.WithTimeout(ctx, taskAwaitTimeout)
	defer cancel()

	info, err := s.client.Index(objectsIndex).DeleteDocumentWithContext(ctx, docID, nil)
	if err != nil {
		return fmt.Errorf("delete from index: %w", err)
	}
	return awaitTask(ctx, s.client, info, "delete "+string(t))
}

// DeleteObjects removes a batch of objects of one type, returning once
// Meilisearch has accepted the task. The retention purge hard-deletes rows
// straight out of Postgres; without this the purged content stays fully
// searchable, which defeats the retention policy entirely.
//
// It deliberately does not await the task: retention calls it from inside the
// purge transaction, holding row locks, where blocking for seconds on an HTTP
// poll would be worse than the residual risk of an accepted-then-failed task.
func (s *Service) DeleteObjects(ctx context.Context, t ObjectType, objectIDs []string) error {
	if len(objectIDs) == 0 {
		return nil
	}
	docIDs := make([]string, 0, len(objectIDs))
	for _, id := range objectIDs {
		docID := DocID(t, id)
		if docID == "" {
			return &PermanentError{Reason: fmt.Sprintf("cannot unindex %s %q: not a known type and uuid", t, id)}
		}
		docIDs = append(docIDs, docID)
	}
	if _, err := s.client.Index(objectsIndex).DeleteDocumentsWithContext(ctx, docIDs, nil); err != nil {
		return fmt.Errorf("delete documents from index: %w", err)
	}
	return nil
}

// IndexMessageAwait, DeleteMessageAwait and DeleteMessages are the message-typed
// entry points the rest of the tree already calls (the worker's indexer, the
// retention purge, the integration harness). They are the message projection of
// the generic methods above, not a second implementation.
func (s *Service) IndexMessageAwait(ctx context.Context, doc MessageDoc) error {
	return s.IndexAwait(ctx, doc.Doc())
}

func (s *Service) DeleteMessageAwait(ctx context.Context, id string) error {
	return s.DeleteAwait(ctx, TypeMessage, id)
}

func (s *Service) DeleteMessages(ctx context.Context, ids []string) error {
	return s.DeleteObjects(ctx, TypeMessage, ids)
}

// DropLegacyIndex deletes the pre-generalization `messages` index. Deleting an
// index that is not there is not an error: the point is that it is gone.
func (s *Service) DropLegacyIndex(ctx context.Context) error {
	if _, err := s.client.GetIndexWithContext(ctx, legacyMessagesIndex); err != nil {
		s.logger.Info("legacy index already absent", "index", legacyMessagesIndex)
		return nil
	}
	task, err := s.client.DeleteIndexWithContext(ctx, legacyMessagesIndex)
	if err != nil {
		return fmt.Errorf("delete legacy index: %w", err)
	}
	if err := awaitTask(ctx, s.client, task, "delete legacy index"); err != nil {
		return err
	}
	s.logger.Info("dropped legacy index", "index", legacyMessagesIndex)
	return nil
}

// --- settings comparison helpers ---

func deref(v *[]string) []string {
	if v == nil {
		return nil
	}
	return *v
}

// attributeNames flattens what GET /settings/filterable-attributes returns.
// Recent Meilisearch versions may answer with rule objects rather than plain
// strings; anything that is not a plain string is rendered so it compares
// unequal to the wanted set and the settings are rewritten.
func attributeNames(attrs *[]interface{}) []string {
	if attrs == nil {
		return nil
	}
	out := make([]string, 0, len(*attrs))
	for _, a := range *attrs {
		if s, ok := a.(string); ok {
			out = append(out, s)
			continue
		}
		out = append(out, fmt.Sprint(a))
	}
	return out
}

func sameSequence(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	return sameSequence(a, b)
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Hit is one search result as a client sees it. It is deliberately not Doc:
// there is no acl field to forget to strip, and no doc_id a client could
// mistake for the object id.
type Hit struct {
	ID          string     `json:"id"`
	Type        ObjectType `json:"type"`
	WorkspaceID string     `json:"workspace_id"`
	ChannelID   string     `json:"channel_id"`
	UserID      string     `json:"user_id"`
	Title       string     `json:"title"`
	Content     string     `json:"content"`
	CreatedAt   int64      `json:"created_at"`
	IsDeleted   bool       `json:"is_deleted"`
}

type SearchResult struct {
	Hits             []Hit `json:"hits"`
	EstimatedTotal   int64 `json:"estimated_total"`
	ProcessingTimeMs int64 `json:"processing_time_ms"`
}

// Query is a fully authorized search request.
//
// Keys is the caller's access key set, resolved by AccessKeys before the query
// is built. It is mandatory: an empty set means the caller may read nothing and
// must get zero results, never an unconstrained workspace-wide search.
//
// Types and ChannelID are narrowings the caller asked for. Neither can widen
// anything — the key set is applied regardless — but both are validated, since
// an unrecognised narrowing that gets dropped silently turns a specific query
// into a broad one.
type Query struct {
	WorkspaceID string
	Text        string
	Keys        []string
	Types       []ObjectType
	ChannelID   string
	FromUserID  string
	Limit       int
}

// buildFilter renders the Meilisearch filter expression for an authorized
// query. ok is false when the query can only ever match nothing, in which case
// the caller must synthesise an empty result instead of searching.
func buildFilter(q Query) (string, bool) {
	wsID, ok := canonicalUUID(q.WorkspaceID)
	if !ok || len(q.Keys) == 0 {
		return "", false
	}

	// An unusable key is dropped; a caller left with none of them searches
	// nothing. This is the single most important branch in the package.
	keys := make([]string, 0, len(q.Keys))
	for _, k := range q.Keys {
		if !validKey(k) {
			continue
		}
		keys = append(keys, quoteFilter(k))
	}
	if len(keys) == 0 {
		return "", false
	}

	var b strings.Builder
	b.WriteString("workspace_id = ")
	b.WriteString(quoteFilter(wsID))
	// Negation in Meilisearch is a set complement, so documents indexed before
	// is_deleted existed are still matched here while soft-deleted ones are not.
	b.WriteString(" AND NOT is_deleted = true")
	b.WriteString(" AND acl IN [")
	b.WriteString(strings.Join(keys, ", "))
	b.WriteString("]")

	if len(q.Types) > 0 {
		types := make([]string, 0, len(q.Types))
		for _, t := range q.Types {
			if !t.Valid() {
				// Dropping it would widen the query to every type.
				return "", false
			}
			types = append(types, quoteFilter(string(t)))
		}
		b.WriteString(" AND type IN [")
		b.WriteString(strings.Join(types, ", "))
		b.WriteString("]")
	}

	if q.ChannelID != "" {
		channelID, ok := canonicalUUID(q.ChannelID)
		if !ok {
			return "", false
		}
		b.WriteString(" AND channel_id = ")
		b.WriteString(quoteFilter(channelID))
	}

	if q.FromUserID != "" {
		from, ok := canonicalUUID(q.FromUserID)
		if !ok {
			return "", false
		}
		b.WriteString(" AND user_id = ")
		b.WriteString(quoteFilter(from))
	}

	return b.String(), true
}

func (s *Service) Search(ctx context.Context, q Query) (*SearchResult, error) {
	filter, ok := buildFilter(q)
	if !ok {
		return &SearchResult{Hits: []Hit{}}, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	idx := s.client.Index(objectsIndex)
	res, err := idx.SearchWithContext(ctx, q.Text, &meilisearch.SearchRequest{
		Filter: filter,
		Limit:  int64(limit),
		Sort:   []string{"created_at:desc"},
		// Never ship the ACL to a client: it names the channels and users an
		// object is shared with. Hit has no field for it either; this is the
		// layer that keeps it off the wire in the first place.
		AttributesToRetrieve: hitAttributes,
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	// Meilisearch hit values come back as json.RawMessage; decode each hit into
	// the typed hit rather than fmt.Sprint-ing raw bytes.
	hits := []Hit{}
	for _, raw := range res.Hits {
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var hit Hit
		if err := json.Unmarshal(b, &hit); err != nil {
			continue
		}
		hits = append(hits, hit)
	}

	return &SearchResult{
		Hits:             hits,
		EstimatedTotal:   res.EstimatedTotalHits,
		ProcessingTimeMs: int64(res.ProcessingTimeMs),
	}, nil
}
