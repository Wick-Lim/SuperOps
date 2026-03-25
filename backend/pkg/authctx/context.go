package authctx

import "context"

type ctxKey string

const (
	keyUserID      ctxKey = "user_id"
	keyWorkspaceID ctxKey = "workspace_id"
	keyRole        ctxKey = "role"
)

func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, keyUserID, userID)
}

func WithWorkspaceID(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, keyWorkspaceID, workspaceID)
}

func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, keyRole, role)
}

func UserID(ctx context.Context) string {
	if v, ok := ctx.Value(keyUserID).(string); ok {
		return v
	}
	return ""
}

func WorkspaceID(ctx context.Context) string {
	if v, ok := ctx.Value(keyWorkspaceID).(string); ok {
		return v
	}
	return ""
}

func Role(ctx context.Context) string {
	if v, ok := ctx.Value(keyRole).(string); ok {
		return v
	}
	return ""
}
