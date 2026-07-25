package push

import "testing"

func TestIsExpoToken(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"ExponentPushToken[xxxxxxxxxxxxxxxxxxxxxx]", true},
		{"ExpoPushToken[xxxxxxxxxxxxxxxxxxxxxx]", true},
		{"ExponentPushToken[a]", true},

		{"", false},
		{"ExponentPushToken[]", false},
		{"ExponentPushToken[abc", false},
		{"abc]", false},
		{"ExponentPushToken", false},
		// A raw APNs/FCM token. Expo will not accept it, so storing it would
		// mean a permanently failing send on every notification for that user.
		{"fA9k2_bare-fcm-token", false},
		// Two tokens concatenated, or an injection attempt at the JSON payload.
		{"ExponentPushToken[a],ExponentPushToken[b]", false},
		{"ExponentPushToken[a][b]", false},
		{" ExponentPushToken[abc]", false},
	}
	for _, tt := range tests {
		if got := IsExpoToken(tt.in); got != tt.want {
			t.Errorf("IsExpoToken(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
