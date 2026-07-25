// Package push delivers notifications to devices that are not currently
// connected — the half of "notification" that a database row and a WebSocket
// frame cannot cover.
//
// The only implementation is Expo's push service (see expo.go), which fronts
// APNs and FCM. That choice follows the client: app/ is an Expo application, so
// the token the device hands us is an Expo token and Expo is the only service
// that can address it.
//
// IMPORTANT: Expo relaying to APNs/FCM still requires real Apple and Google
// credentials uploaded to the Expo project. Without them Expo accepts the
// message and answers with a per-message `InvalidCredentials` receipt, which
// this package logs. Push being "enabled" here is necessary, not sufficient.
package push

import (
	"context"
	"strings"
)

// MaxBatchSize is the number of messages Expo accepts in one request. Anything
// larger is rejected outright, so Sender implementations must split.
const MaxBatchSize = 100

// Message is one notification addressed to one device token.
type Message struct {
	// Token is the device's Expo push token (`ExponentPushToken[...]`).
	Token string
	Title string
	Body  string
	// Data is the payload handed to the client when the notification is tapped.
	// It carries channel_id/message_id so the app can open the right thread.
	Data map[string]string
	// Badge is the app icon badge to set, or nil to leave it alone.
	Badge *int
}

// Sender delivers a batch of push messages.
//
// A returned error means "some or all of this batch failed"; it is not a signal
// that any particular message was or was not delivered, because a batch can be
// partially accepted. Implementations report per-message outcomes out of band
// (ExpoSender logs them, and routes dead tokens to its OnInvalidTokens hook).
type Sender interface {
	Send(ctx context.Context, msgs []Message) error
}

// InvalidTokenFunc receives tokens the push service has reported as dead
// (`DeviceNotRegistered`: the app was uninstalled, or the OS rotated the
// token). Deleting them is not optional housekeeping — a dead token is retried
// on every single notification forever otherwise, and it never starts working
// again.
type InvalidTokenFunc func(ctx context.Context, tokens []string)

// IsExpoToken reports whether s has the shape expo-notifications produces.
//
// Registration validates with this so a client bug cannot fill device_tokens
// with values no push service will ever accept — every one of which would then
// be sent, rejected and retried on every notification for that user.
func IsExpoToken(s string) bool {
	for _, prefix := range []string{"ExponentPushToken[", "ExpoPushToken["} {
		if strings.HasPrefix(s, prefix) && strings.HasSuffix(s, "]") && len(s) > len(prefix)+1 {
			// The interior is opaque, but it must not contain the delimiters or
			// this is not one token.
			inner := s[len(prefix) : len(s)-1]
			return !strings.ContainsAny(inner, "[]")
		}
	}
	return false
}
