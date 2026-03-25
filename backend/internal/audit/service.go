package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Log(ctx context.Context, workspaceID, actorID, action, resourceType, resourceID string, metadata map[string]interface{}) error {
	metaJSON, _ := json.Marshal(metadata)
	if metaJSON == nil {
		metaJSON = []byte("{}")
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_logs (id, workspace_id, actor_id, action, resource_type, resource_id, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		uuid.NewString(), nilIfEmpty(workspaceID), nilIfEmpty(actorID), action, resourceType, nilIfEmpty(resourceID), string(metaJSON),
	)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
