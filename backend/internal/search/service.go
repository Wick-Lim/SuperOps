package search

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/meilisearch/meilisearch-go"
)

const messagesIndex = "messages"

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

	// Ensure index exists
	_, err := client.GetIndex(messagesIndex)
	if err != nil {
		_, err = client.CreateIndex(&meilisearch.IndexConfig{
			Uid:        messagesIndex,
			PrimaryKey: "id",
		})
		if err != nil {
			return nil, fmt.Errorf("create index: %w", err)
		}

		// Configure searchable/filterable attributes
		idx := client.Index(messagesIndex)
		idx.UpdateSearchableAttributes(&[]string{"content"})
		filterable := []interface{}{"channel_id", "workspace_id", "user_id", "created_at"}
		idx.UpdateFilterableAttributes(&filterable)
		sortable := []string{"created_at"}
		idx.UpdateSortableAttributes(&sortable)

		logger.Info("created Meilisearch index", "index", messagesIndex)
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

type SearchResult struct {
	Hits             []MessageDoc `json:"hits"`
	EstimatedTotal   int64        `json:"estimated_total"`
	ProcessingTimeMs int64        `json:"processing_time_ms"`
}

func (s *Service) Search(ctx context.Context, workspaceID, query string, channelID, userID string, limit int) (*SearchResult, error) {
	idx := s.client.Index(messagesIndex)

	filter := fmt.Sprintf("workspace_id = \"%s\"", workspaceID)
	if channelID != "" {
		filter += fmt.Sprintf(" AND channel_id = \"%s\"", channelID)
	}
	if userID != "" {
		filter += fmt.Sprintf(" AND user_id = \"%s\"", userID)
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

	var hits []MessageDoc
	for _, hit := range res.Hits {
		doc := MessageDoc{
			ID:          fmt.Sprint(hit["id"]),
			ChannelID:   fmt.Sprint(hit["channel_id"]),
			WorkspaceID: fmt.Sprint(hit["workspace_id"]),
			UserID:      fmt.Sprint(hit["user_id"]),
			Content:     fmt.Sprint(hit["content"]),
		}
		hits = append(hits, doc)
	}

	return &SearchResult{
		Hits:             hits,
		EstimatedTotal:   res.EstimatedTotalHits,
		ProcessingTimeMs: int64(res.ProcessingTimeMs),
	}, nil
}
