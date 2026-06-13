package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/meilisearch/meilisearch-go"
)

const messagesIndex = "messages"

// quoteFilter escapes a value for safe interpolation into a Meilisearch filter
// expression (defends against breaking out of the quoted string).
func quoteFilter(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + v + `"`
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
	idx.UpdateSearchableAttributes(&[]string{"content"})
	filterable := []interface{}{"channel_id", "workspace_id", "user_id", "created_at"}
	idx.UpdateFilterableAttributes(&filterable)
	sortable := []string{"created_at"}
	idx.UpdateSortableAttributes(&sortable)

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

type SearchResult struct {
	Hits             []MessageDoc `json:"hits"`
	EstimatedTotal   int64        `json:"estimated_total"`
	ProcessingTimeMs int64        `json:"processing_time_ms"`
}

func (s *Service) Search(ctx context.Context, workspaceID, query string, channelID, userID string, limit int) (*SearchResult, error) {
	idx := s.client.Index(messagesIndex)

	filter := "workspace_id = " + quoteFilter(workspaceID)
	if channelID != "" {
		filter += " AND channel_id = " + quoteFilter(channelID)
	}
	if userID != "" {
		filter += " AND user_id = " + quoteFilter(userID)
	}

	if limit == 0 {
		limit = 20
	}

	res, err := idx.Search(query, &meilisearch.SearchRequest{
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
