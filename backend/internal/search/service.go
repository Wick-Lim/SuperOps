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

const messagesIndex = "messages"

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
)

// quoteFilter escapes a value for safe interpolation into a Meilisearch filter
// expression (defends against breaking out of the quoted string). Every value
// that reaches it is already canonicalised to a UUID by canonicalUUID, so this
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

type MessageDoc struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Content     string `json:"content"`
	CreatedAt   int64  `json:"created_at"`
	// IsDeleted mirrors messages.is_deleted so soft-deleted content stops being
	// searchable. Documents written before this field existed simply lack it,
	// which the "NOT is_deleted = true" filter still treats as visible.
	IsDeleted bool `json:"is_deleted"`
}

// wantSearchable, wantFilterable and wantSortable are the index settings this
// build requires. wantFilterable in particular is load-bearing for
// authorization: buildFilter constrains every query by workspace_id and an
// explicit channel_id allowlist, and Meilisearch rejects a filter on an
// attribute that is not filterable. Getting that set wrong therefore does not
// widen search results, it breaks search outright — which is exactly why the
// update must be verified rather than fired and forgotten.
var (
	wantSearchable = []string{"content"}
	wantFilterable = []string{"channel_id", "workspace_id", "user_id", "created_at", "is_deleted"}
	wantSortable   = []string{"created_at"}
)

func NewService(host, masterKey string, logger *slog.Logger) (*Service, error) {
	client := meilisearch.New(host, meilisearch.WithAPIKey(masterKey))

	ctx, cancel := context.WithTimeout(context.Background(), settingsTimeout)
	defer cancel()

	// Ensure index exists.
	if _, err := client.GetIndexWithContext(ctx, messagesIndex); err != nil {
		task, createErr := client.CreateIndexWithContext(ctx, &meilisearch.IndexConfig{
			Uid:        messagesIndex,
			PrimaryKey: "id",
		})
		if createErr != nil {
			return nil, fmt.Errorf("create index: %w", createErr)
		}
		if err := awaitTask(ctx, client, task, "create index"); err != nil {
			// Losing the create race against another booting process is not a
			// failure: what matters is that the index is there now.
			if _, exists := client.GetIndexWithContext(ctx, messagesIndex); exists != nil {
				return nil, err
			}
		} else {
			logger.Info("created Meilisearch index", "index", messagesIndex)
		}
	}

	s := &Service{client: client, logger: logger}
	if err := s.ensureSettings(ctx); err != nil {
		return nil, err
	}

	logger.Info("connected to Meilisearch", "host", host)
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
	idx := s.client.Index(messagesIndex)

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

// IndexMessage enqueues a document upsert and returns as soon as Meilisearch
// has accepted the task.
//
// Bulk callers (cmd/reindex) want this form: waiting on each of a million
// documents at ~50ms a piece would take days. A caller that has to decide
// whether the write really happened — the durable indexer, which acks a
// JetStream message on the strength of it — must use IndexMessageAwait.
func (s *Service) IndexMessage(doc MessageDoc) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskAwaitTimeout)
	defer cancel()
	_, err := s.enqueueIndex(ctx, doc)
	return err
}

// IndexMessageAwait upserts a document and waits for Meilisearch to apply it.
// The wait is capped so a Meilisearch that has stopped draining its queue
// produces a retry rather than pinning the caller until its own deadline.
func (s *Service) IndexMessageAwait(ctx context.Context, doc MessageDoc) error {
	ctx, cancel := context.WithTimeout(ctx, taskAwaitTimeout)
	defer cancel()

	info, err := s.enqueueIndex(ctx, doc)
	if err != nil {
		return err
	}
	return awaitTask(ctx, s.client, info, "index message")
}

func (s *Service) enqueueIndex(ctx context.Context, doc MessageDoc) (*meilisearch.TaskInfo, error) {
	info, err := s.client.Index(messagesIndex).AddDocumentsWithContext(ctx, []MessageDoc{doc}, nil)
	if err != nil {
		return nil, fmt.Errorf("index message: %w", err)
	}
	return info, nil
}

// DeleteMessageAwait removes one document and waits for the deletion to be
// applied. Deleting an id that is not in the index is a success, so the durable
// indexer can safely be redelivered the same deletion.
func (s *Service) DeleteMessageAwait(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, taskAwaitTimeout)
	defer cancel()

	info, err := s.client.Index(messagesIndex).DeleteDocumentWithContext(ctx, id, nil)
	if err != nil {
		return fmt.Errorf("delete from index: %w", err)
	}
	return awaitTask(ctx, s.client, info, "delete message")
}

// DeleteMessages removes a batch of documents, returning once Meilisearch has
// accepted the task. The retention purge hard-deletes rows straight out of
// Postgres; without this the purged content stays fully searchable, which
// defeats the retention policy entirely.
//
// It deliberately does not await the task: retention calls it from inside the
// purge transaction, holding row locks, where blocking for seconds on an HTTP
// poll would be worse than the residual risk of an accepted-then-failed task.
func (s *Service) DeleteMessages(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.client.Index(messagesIndex).DeleteDocumentsWithContext(ctx, ids, nil); err != nil {
		return fmt.Errorf("delete documents from index: %w", err)
	}
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

type SearchResult struct {
	Hits             []MessageDoc `json:"hits"`
	EstimatedTotal   int64        `json:"estimated_total"`
	ProcessingTimeMs int64        `json:"processing_time_ms"`
}

// Query is a fully authorized search request.
//
// ChannelIDs is the set of channels the caller is allowed to read, resolved by
// authz.ReadableChannelIDs before the query is built. It is mandatory: an empty
// set means the caller may read nothing and must get zero results, never an
// unconstrained workspace-wide search.
type Query struct {
	WorkspaceID string
	Text        string
	ChannelIDs  []string
	FromUserID  string
	Limit       int
}

// buildFilter renders the Meilisearch filter expression for an authorized
// query. ok is false when the query can only ever match nothing, in which case
// the caller must synthesise an empty result instead of searching.
func buildFilter(q Query) (string, bool) {
	wsID, ok := canonicalUUID(q.WorkspaceID)
	if !ok || len(q.ChannelIDs) == 0 {
		return "", false
	}

	ids := make([]string, 0, len(q.ChannelIDs))
	for _, id := range q.ChannelIDs {
		c, ok := canonicalUUID(id)
		if !ok {
			continue
		}
		ids = append(ids, quoteFilter(c))
	}
	if len(ids) == 0 {
		return "", false
	}

	var b strings.Builder
	b.WriteString("workspace_id = ")
	b.WriteString(quoteFilter(wsID))
	// Negation in Meilisearch is a set complement, so documents indexed before
	// is_deleted existed are still matched here while soft-deleted ones are not.
	b.WriteString(" AND NOT is_deleted = true")
	b.WriteString(" AND channel_id IN [")
	b.WriteString(strings.Join(ids, ", "))
	b.WriteString("]")

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
		return &SearchResult{Hits: []MessageDoc{}}, nil
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}

	idx := s.client.Index(messagesIndex)
	res, err := idx.SearchWithContext(ctx, q.Text, &meilisearch.SearchRequest{
		Filter: filter,
		Limit:  int64(limit),
		Sort:   []string{"created_at:desc"},
	})
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	// Meilisearch hit values come back as json.RawMessage; decode each hit into
	// the typed doc rather than fmt.Sprint-ing raw bytes.
	hits := []MessageDoc{}
	for _, hit := range res.Hits {
		b, err := json.Marshal(hit)
		if err != nil {
			continue
		}
		var doc MessageDoc
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		hits = append(hits, doc)
	}

	return &SearchResult{
		Hits:             hits,
		EstimatedTotal:   res.EstimatedTotalHits,
		ProcessingTimeMs: int64(res.ProcessingTimeMs),
	}, nil
}
