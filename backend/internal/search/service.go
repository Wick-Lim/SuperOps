package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/meilisearch/meilisearch-go"
)

const messagesIndex = "messages"

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

func NewService(host, masterKey string, logger *slog.Logger) (*Service, error) {
	client := meilisearch.New(host, meilisearch.WithAPIKey(masterKey))

	// Ensure index exists.
	if _, err := client.GetIndex(messagesIndex); err != nil {
		if _, err = client.CreateIndex(&meilisearch.IndexConfig{
			Uid:        messagesIndex,
			PrimaryKey: "id",
		}); err != nil {
			return nil, fmt.Errorf("create index: %w", err)
		}
		logger.Info("created Meilisearch index", "index", messagesIndex)
	}

	// Apply attribute settings idempotently on every startup so schema changes
	// take effect for existing indexes too.
	idx := client.Index(messagesIndex)
	if _, err := idx.UpdateSearchableAttributes(&[]string{"content"}); err != nil {
		return nil, fmt.Errorf("update searchable attributes: %w", err)
	}
	filterable := []interface{}{"channel_id", "workspace_id", "user_id", "created_at", "is_deleted"}
	if _, err := idx.UpdateFilterableAttributes(&filterable); err != nil {
		return nil, fmt.Errorf("update filterable attributes: %w", err)
	}
	sortable := []string{"created_at"}
	if _, err := idx.UpdateSortableAttributes(&sortable); err != nil {
		return nil, fmt.Errorf("update sortable attributes: %w", err)
	}

	logger.Info("connected to Meilisearch", "host", host)
	return &Service{client: client, logger: logger}, nil
}

func (s *Service) IndexMessage(doc MessageDoc) error {
	idx := s.client.Index(messagesIndex)
	_, err := idx.AddDocuments([]MessageDoc{doc}, nil)
	if err != nil {
		return fmt.Errorf("index message: %w", err)
	}
	return nil
}

func (s *Service) DeleteMessage(id string) error {
	idx := s.client.Index(messagesIndex)
	_, err := idx.DeleteDocument(id, nil)
	if err != nil {
		return fmt.Errorf("delete from index: %w", err)
	}
	return nil
}

// DeleteMessages removes a batch of documents. The retention purge hard-deletes
// rows straight out of Postgres; without this the purged content stays fully
// searchable, which defeats the retention policy entirely.
func (s *Service) DeleteMessages(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	idx := s.client.Index(messagesIndex)
	if _, err := idx.DeleteDocuments(ids, nil); err != nil {
		return fmt.Errorf("delete documents from index: %w", err)
	}
	return nil
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
