package channel

import "testing"

// The partial unique index idx_channels_dm_key is the only thing preventing
// duplicate 1:1 DMs, and it only works if both participants compute the same
// key. Argument order therefore must not matter, and the result must match the
// SQL expression migration 009 backfilled with:
// MIN(user_id::text) || ':' || MAX(user_id::text).
func TestDMKeyIsOrderIndependent(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{
			name: "already sorted",
			a:    "00000000-0000-0000-0000-00000000000a",
			b:    "00000000-0000-0000-0000-00000000000b",
			want: "00000000-0000-0000-0000-00000000000a:00000000-0000-0000-0000-00000000000b",
		},
		{
			name: "reversed",
			a:    "00000000-0000-0000-0000-00000000000b",
			b:    "00000000-0000-0000-0000-00000000000a",
			want: "00000000-0000-0000-0000-00000000000a:00000000-0000-0000-0000-00000000000b",
		},
		{
			name: "lexicographic, not numeric",
			a:    "9f000000-0000-0000-0000-000000000000",
			b:    "10000000-0000-0000-0000-000000000000",
			want: "10000000-0000-0000-0000-000000000000:9f000000-0000-0000-0000-000000000000",
		},
		{
			name: "identical ids",
			a:    "aaaaaaaa-0000-0000-0000-000000000000",
			b:    "aaaaaaaa-0000-0000-0000-000000000000",
			want: "aaaaaaaa-0000-0000-0000-000000000000:aaaaaaaa-0000-0000-0000-000000000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DMKey(tt.a, tt.b); got != tt.want {
				t.Errorf("DMKey(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
			if got := DMKey(tt.b, tt.a); got != tt.want {
				t.Errorf("DMKey(%q, %q) = %q, want %q (must be order-independent)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

// The ':' separator is only unambiguous because participants are uuids, which
// cannot contain one. That is the same assumption migration 009 makes, so the
// format must not be "hardened" away from it.
func TestDMKeyDistinctPairsDoNotCollide(t *testing.T) {
	a := "11111111-1111-1111-1111-111111111111"
	b := "22222222-2222-2222-2222-222222222222"
	c := "33333333-3333-3333-3333-333333333333"

	seen := map[string]string{}
	for _, pair := range [][2]string{{a, b}, {b, c}, {a, c}} {
		key := DMKey(pair[0], pair[1])
		if prev, dup := seen[key]; dup {
			t.Fatalf("dm_key %q produced by both %s and %s+%s", key, prev, pair[0], pair[1])
		}
		seen[key] = pair[0] + "+" + pair[1]
	}
}

func TestChannelTypeIsDM(t *testing.T) {
	tests := []struct {
		in   ChannelType
		want bool
	}{
		{TypeDM, true},
		{TypeGroupDM, true},
		{TypePublic, false},
		{TypePrivate, false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			if got := tt.in.IsDM(); got != tt.want {
				t.Errorf("ChannelType(%q).IsDM() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidNotificationPref(t *testing.T) {
	for _, p := range []string{NotifyAll, NotifyMentions, NotifyNone, NotifyDefault} {
		if !validNotificationPref(p) {
			t.Errorf("validNotificationPref(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "ALL", "some", "mention"} {
		if validNotificationPref(p) {
			t.Errorf("validNotificationPref(%q) = true, want false", p)
		}
	}
}
