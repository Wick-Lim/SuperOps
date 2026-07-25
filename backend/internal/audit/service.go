package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Common actions. Keep the "<resource>.<verb>" shape when adding more.
const (
	ActionLogin           = "user.login"
	ActionLoginFailed     = "user.login_failed"
	ActionLogout          = "user.logout"
	ActionPasswordChanged = "user.password_changed"
	ActionTOTPEnabled     = "user.totp_enabled"
	ActionTOTPDisabled    = "user.totp_disabled"
	ActionInviteAccepted  = "invitation.accepted"
)

// Entry is a single audit record. Action and ResourceType are required; every
// other field is optional and empty IDs are persisted as NULL.
type Entry struct {
	WorkspaceID  string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	// IPAddress is the caller's address. Anything that is not a parsable IP
	// literal (including "") is stored as NULL rather than failing the insert.
	IPAddress string
	Metadata  map[string]interface{}
}

type Service struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, logger: logger}
}

// Record writes one audit entry.
func (s *Service) Record(ctx context.Context, e Entry) error {
	metaJSON, err := json.Marshal(e.Metadata)
	if err != nil || metaJSON == nil {
		metaJSON = []byte("{}")
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO audit_logs (id, workspace_id, actor_id, action, resource_type, resource_id, metadata, ip_address)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::inet)`,
		uuid.NewString(), nilIfEmpty(e.WorkspaceID), nilIfEmpty(e.ActorID), e.Action,
		e.ResourceType, nilIfEmpty(e.ResourceID), string(metaJSON), nilIfNotIP(e.IPAddress),
	); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

// Try records an entry, logging rather than returning a failure. Use it once
// the audited operation has already committed: losing the trail entry must be
// visible in the logs, but it must not turn a successful login into a 500.
//
// The write is detached from the caller's cancellation so that a client which
// hangs up immediately after (say) a failed login still leaves a record.
func (s *Service) Try(ctx context.Context, e Entry) {
	if err := s.Record(context.WithoutCancel(ctx), e); err != nil {
		s.logger.Error("audit log write failed",
			"error", err, "action", e.Action, "actor_id", e.ActorID, "resource_id", e.ResourceID)
	}
}

// Log is the positional form of Record, kept for existing call sites.
func (s *Service) Log(ctx context.Context, workspaceID, actorID, action, resourceType, resourceID string, metadata map[string]interface{}) error {
	return s.Record(ctx, Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     metadata,
	})
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nilIfNotIP(s string) interface{} {
	if net.ParseIP(s) == nil {
		return nil
	}
	return s
}
