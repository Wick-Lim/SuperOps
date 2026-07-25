package user

import (
	"context"
	"fmt"
)

// MaxDeviceTokenLen mirrors the CHECK constraint added in migration 012. It is
// validated in the handler so an over-long token is a 400 rather than a 500.
const MaxDeviceTokenLen = 512

// Device platforms accepted by the device_tokens CHECK constraint. Anything
// else a client sends is normalised to PlatformUnknown rather than rejected:
// the platform is a diagnostic, and refusing the registration over it would
// cost the user their push notifications.
const (
	PlatformIOS     = "ios"
	PlatformAndroid = "android"
	PlatformWeb     = "web"
	PlatformUnknown = "unknown"
)

// NormalizePlatform maps a client-supplied platform onto the allowed set.
func NormalizePlatform(p string) string {
	switch p {
	case PlatformIOS, PlatformAndroid, PlatformWeb:
		return p
	default:
		return PlatformUnknown
	}
}

// RegisterDevice records a device's push token for userID, moving the token
// off whatever user held it before.
//
// The reassignment is the entire reason this is an upsert on `token` rather
// than on `(user_id, token)`. A push token identifies a *device*, and devices
// are shared: when B signs in on A's phone, the OS hands the app the same token
// it previously gave A. If A's row survived, every notification addressed to A
// would be delivered to a handset that is now logged in as B — one user reading
// another's DMs on their lock screen. `ON CONFLICT (token) DO UPDATE SET
// user_id = EXCLUDED.user_id` makes the handover atomic: there is never an
// instant where two users own the same token, and there is no window where the
// row is missing either.
//
// created_at is reset only on an actual change of owner, so it keeps meaning
// "when this device started belonging to this user" rather than "when the app
// last launched" — which is what last_seen_at is for.
func (r *Repository) RegisterDevice(ctx context.Context, userID, token, platform string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO device_tokens (user_id, token, platform)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (token) DO UPDATE
		    SET user_id      = EXCLUDED.user_id,
		        platform     = EXCLUDED.platform,
		        created_at   = CASE WHEN device_tokens.user_id <> EXCLUDED.user_id
		                            THEN NOW() ELSE device_tokens.created_at END,
		        last_seen_at = NOW()`,
		userID, token, NormalizePlatform(platform))
	if err != nil {
		return fmt.Errorf("register device token: %w", err)
	}
	return nil
}

// DeleteDevice removes one of the caller's registered tokens and reports
// whether a row was actually removed.
//
// It is scoped to user_id so a client cannot deregister a token it does not
// hold. A token that has already been reassigned to somebody else therefore
// answers "not found" instead of unregistering that other user's device.
func (r *Repository) DeleteDevice(ctx context.Context, userID, token string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM device_tokens WHERE user_id = $1 AND token = $2`, userID, token)
	if err != nil {
		return false, fmt.Errorf("delete device token: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteDeviceTokens removes tokens the push service has reported as dead,
// whoever currently owns them.
//
// Unlike DeleteDevice this is deliberately not scoped to a user: the caller is
// the push pipeline reacting to a `DeviceNotRegistered` receipt, which is a
// statement about the device, not about a session. Without it a dead token is
// re-sent and re-rejected on every notification for that user, forever.
func (r *Repository) DeleteDeviceTokens(ctx context.Context, tokens []string) (int64, error) {
	if len(tokens) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM device_tokens WHERE token = ANY($1)`, tokens)
	if err != nil {
		return 0, fmt.Errorf("delete device tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PushTokensForUser lists the device tokens a notification for userID should be
// delivered to. It satisfies the notification service's DeviceTokenLister.
func (r *Repository) PushTokensForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT token FROM device_tokens WHERE user_id = $1 ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	defer rows.Close()

	tokens := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan device token: %w", err)
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list device tokens: %w", err)
	}
	return tokens, nil
}
