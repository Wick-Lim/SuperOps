package authz

import (
	"sort"
	"testing"
)

// --- workspace ---------------------------------------------------------------

func TestWorkspaceRole(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name        string
		workspaceID string
		userID      string
		want        string
	}{
		{"owner", w.wsA, w.owner, RoleOwner},
		{"admin", w.wsA, w.admin, RoleAdmin},
		{"member", w.wsA, w.member, RoleMember},
		{"guest", w.wsA, w.guest, RoleGuest},
		// A non-member is not an error: the caller must be able to tell "no"
		// from "the database is down".
		{"non-member of this workspace", w.wsA, w.stranger, ""},
		{"member of another workspace only", w.wsB, w.member, ""},
		{"workspace does not exist", missingID, w.owner, ""},
		{"user does not exist", w.wsA, missingID, ""},
		// Empty ids short-circuit before the query; without that they would hit
		// a uuid parse error and turn a missing path parameter into a 500.
		{"empty workspace id", "", w.owner, ""},
		{"empty user id", w.wsA, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.WorkspaceRole(t.Context(), tt.workspaceID, tt.userID)
			if err != nil {
				t.Fatalf("WorkspaceRole: unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("WorkspaceRole = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWorkspacePredicates is the workspace half of the tenancy matrix, asserted
// through the API the product actually enforces — Can, with a capability — and
// against the predicate Can is composed from, in the same table.
//
// Keeping both columns in one test is what replaced the dual-run comparison
// when step 5 deleted the legacy methods. The comparison could only ever check
// the pairs that real traffic happened to generate; this checks every pair,
// deterministically, on every run, with no configuration flag.
func TestWorkspacePredicates(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name                       string
		workspaceID, userID        string
		isMember, isAdmin, isOwner bool
	}{
		{"owner is member, admin and owner", w.wsA, w.owner, true, true, true},
		{"admin is member and admin but not owner", w.wsA, w.admin, true, true, false},
		{"member is only a member", w.wsA, w.member, true, false, false},
		{"guest is a member but never an admin", w.wsA, w.guest, true, false, false},
		{"stranger is nothing", w.wsA, w.stranger, false, false, false},
		{"owner of wsB is nothing in wsA", w.wsA, w.stranger, false, false, false},
		{"owner of wsA is nothing in wsB", w.wsB, w.owner, false, false, false},
		{"missing workspace", missingID, w.owner, false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			subject := UserSubject(tt.userID)
			obj := WorkspaceObject(tt.workspaceID)
			if got, err := az.Can(ctx, subject, obj, CapRead); err != nil || got != tt.isMember {
				t.Errorf("Can(read) = %v, %v; want %v, nil", got, err, tt.isMember)
			}
			if got, err := az.Can(ctx, subject, obj, CapAdmin); err != nil || got != tt.isAdmin {
				t.Errorf("Can(admin) = %v, %v; want %v, nil", got, err, tt.isAdmin)
			}
			// The predicate Can composes must give the same answer. This is the
			// equivalence the dual-run comparison used to assert on live traffic.
			if got, err := az.isWorkspaceMember(ctx, tt.workspaceID, tt.userID); err != nil || got != tt.isMember {
				t.Errorf("isWorkspaceMember = %v, %v; want %v, nil", got, err, tt.isMember)
			}
			if got, err := az.IsWorkspaceOwner(ctx, tt.workspaceID, tt.userID); err != nil || got != tt.isOwner {
				t.Errorf("IsWorkspaceOwner = %v, %v; want %v, nil", got, err, tt.isOwner)
			}
		})
	}
}

func TestAdminWorkspaceIDs(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name   string
		userID string
		want   []string
	}{
		{"owner administers their workspace", w.owner, []string{w.wsA}},
		{"admin administers their workspace", w.admin, []string{w.wsA}},
		{"admin of two tenants gets both", w.multiAdmin, []string{w.wsA, w.wsB}},
		// The whole point of the function: a plain member is an admin nowhere,
		// so an /admin/* query scoped to this list matches nothing.
		{"plain member administers nothing", w.member, nil},
		{"guest administers nothing", w.guest, nil},
		{"unknown user administers nothing", missingID, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.AdminWorkspaceIDs(t.Context(), tt.userID)
			if err != nil {
				t.Fatalf("AdminWorkspaceIDs: unexpected error %v", err)
			}
			// An empty result must be an empty slice, never nil: it is passed
			// straight into `workspace_id = ANY($1)`, and nil there is NULL,
			// which is not the same query.
			if got == nil {
				t.Fatal("AdminWorkspaceIDs returned nil; want an empty slice")
			}
			if !sameSet(got, tt.want) {
				t.Errorf("AdminWorkspaceIDs = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSharesWorkspaceIsAsymmetric pins the deliberate asymmetry: the ACTOR must
// be an owner/admin. /api/v1/admin/* uses this to decide whether a caller may
// touch another account, so making it symmetric would hand every plain member
// of a workspace the admin surface over their colleagues.
func TestSharesWorkspaceIsAsymmetric(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name          string
		actor, target string
		want          bool
	}{
		{"owner over a member", w.owner, w.member, true},
		{"admin over a guest", w.admin, w.guest, true},
		{"owner over another admin", w.owner, w.admin, true},
		{"owner over themselves", w.owner, w.owner, true},

		// Reverse of the first two rows. Same pair, same workspace, opposite
		// answer — that is the asymmetry.
		{"member over the owner", w.member, w.owner, false},
		{"guest over the admin", w.guest, w.admin, false},
		{"member over another member", w.member, w.blocker, false},
		{"member over themselves", w.member, w.member, false},

		{"owner over a different tenant", w.owner, w.stranger, false},
		{"owner over an unknown user", w.owner, missingID, false},
		{"unknown actor", missingID, w.member, false},

		// multiAdmin administers both tenants, so it reaches accounts in each.
		{"admin of two tenants reaches wsA", w.multiAdmin, w.member, true},
		{"admin of two tenants reaches wsB", w.multiAdmin, w.stranger, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.SharesWorkspace(t.Context(), tt.actor, tt.target)
			if err != nil {
				t.Fatalf("SharesWorkspace: unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("SharesWorkspace(%s, %s) = %v, want %v", tt.actor, tt.target, got, tt.want)
			}
		})
	}
}

// --- channel resolution ------------------------------------------------------

func TestChannel(t *testing.T) {
	az, w := seed(t)

	t.Run("resolves the workspace and type", func(t *testing.T) {
		got, err := az.Channel(t.Context(), w.chPrivate)
		if err != nil {
			t.Fatalf("Channel: unexpected error %v", err)
		}
		if got == nil {
			t.Fatal("Channel returned nil for an existing channel")
		}
		if got.ID != w.chPrivate || got.WorkspaceID != w.wsA || got.Type != "private" || got.IsArchived {
			t.Errorf("Channel = %+v, want id=%s workspace=%s type=private archived=false",
				got, w.chPrivate, w.wsA)
		}
	})

	t.Run("reports archived state", func(t *testing.T) {
		got, err := az.Channel(t.Context(), w.chArchived)
		if err != nil {
			t.Fatalf("Channel: unexpected error %v", err)
		}
		if got == nil || !got.IsArchived {
			t.Errorf("Channel = %+v, want IsArchived true", got)
		}
	})

	t.Run("missing channel is (nil, nil)", func(t *testing.T) {
		got, err := az.Channel(t.Context(), missingID)
		if err != nil {
			t.Fatalf("Channel: unexpected error %v", err)
		}
		if got != nil {
			t.Errorf("Channel = %+v, want nil", got)
		}
	})
}

func TestMessageChannel(t *testing.T) {
	az, w := seed(t)

	t.Run("resolves the channel that owns the message", func(t *testing.T) {
		// This is what stops a caller authorizing against a channel id they
		// chose in the URL rather than the one the message actually lives in.
		got, err := az.MessageChannel(t.Context(), w.msgInPrivate)
		if err != nil {
			t.Fatalf("MessageChannel: unexpected error %v", err)
		}
		if got == nil {
			t.Fatal("MessageChannel returned nil for an existing message")
		}
		if got.ID != w.chPrivate || got.WorkspaceID != w.wsA || got.Type != "private" {
			t.Errorf("MessageChannel = %+v, want the private channel in wsA", got)
		}
	})

	t.Run("missing message is (nil, nil)", func(t *testing.T) {
		got, err := az.MessageChannel(t.Context(), missingID)
		if err != nil {
			t.Fatalf("MessageChannel: unexpected error %v", err)
		}
		if got != nil {
			t.Errorf("MessageChannel = %+v, want nil", got)
		}
	})
}

func TestChannelRoleAndPredicates(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name              string
		channelID, userID string
		wantRole          string
		wantMember        bool
		wantAdmin         bool
	}{
		{"channel admin", w.chPublic, w.owner, ChannelRoleAdmin, true, true},
		{"channel member", w.chPrivate, w.member, ChannelRoleMember, true, false},
		{"dm participant", w.chDM, w.admin, ChannelRoleMember, true, false},
		// A workspace admin is not automatically a channel member; the two
		// vocabularies are separate on purpose.
		{"workspace admin who never joined", w.chPrivate, w.admin, "", false, false},
		{"stranger", w.chPublic, w.stranger, "", false, false},
		{"channel with no members at all", w.chPublicEmpty, w.owner, "", false, false},
		{"missing channel", missingID, w.owner, "", false, false},
		{"empty channel id", "", w.owner, "", false, false},
		{"empty user id", w.chPublic, "", "", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			role, err := az.ChannelRole(ctx, tt.channelID, tt.userID)
			if err != nil {
				t.Fatalf("ChannelRole: unexpected error %v", err)
			}
			if role != tt.wantRole {
				t.Errorf("ChannelRole = %q, want %q", role, tt.wantRole)
			}
			// Membership is CapWrite on the ladder, not CapRead: a non-member of
			// a public channel holds CapRead on it and must not be counted here.
			subject := UserSubject(tt.userID)
			obj := ChannelObject(tt.channelID)
			if got, err := az.Can(ctx, subject, obj, CapWrite); err != nil || got != tt.wantMember {
				t.Errorf("Can(write) = %v, %v; want %v, nil", got, err, tt.wantMember)
			}
			if got, err := az.Can(ctx, subject, obj, CapAdmin); err != nil || got != tt.wantAdmin {
				t.Errorf("Can(admin) = %v, %v; want %v, nil", got, err, tt.wantAdmin)
			}
			if got, err := az.isChannelMember(ctx, tt.channelID, tt.userID); err != nil || got != tt.wantMember {
				t.Errorf("isChannelMember = %v, %v; want %v, nil", got, err, tt.wantMember)
			}
		})
	}
}

// TestChannelReadability is the load-bearing one. The "public channel readable
// by a stranger" row is the exact gap that allowed cross-tenant browsing, and
// every row here is asserted twice: through Can, which is what handlers
// enforce, and through canReadChannel, the predicate Can is composed from.
func TestChannelReadability(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name      string
		channelID string
		userID    string
		want      bool
	}{
		{"channel member reads their public channel", w.chPublic, w.owner, true},
		{"channel member reads their private channel", w.chPrivate, w.member, true},
		{"dm participant reads the dm", w.chDM, w.owner, true},

		{"workspace member reads a public channel they have not joined", w.chPublic, w.member, true},
		{"guest reads a public channel", w.chPublic, w.guest, true},
		{"workspace member reads a public channel with no members", w.chPublicEmpty, w.guest, true},
		{"archived public channels stay readable by workspace members", w.chArchived, w.member, true},

		// The regression this package exists for: membership of *some*
		// workspace is not membership of THIS one.
		{"stranger cannot read a public channel", w.chPublic, w.stranger, false},
		{"stranger cannot read an empty public channel", w.chPublicEmpty, w.stranger, false},
		{"wsA owner cannot read wsB's public channel", w.chOtherTenant, w.owner, false},

		{"workspace admin cannot read a private channel they have not joined", w.chPrivate, w.admin, false},
		{"workspace owner cannot read a private channel they have not joined", w.chPrivate, w.owner, false},
		{"workspace member cannot read a dm they are not in", w.chDM, w.member, false},
		{"stranger cannot read a private channel", w.chPrivate, w.stranger, false},
		{"empty user id reads nothing", w.chPublic, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			got, err := az.Can(ctx, UserSubject(tt.userID), ChannelObject(tt.channelID), CapRead)
			if err != nil {
				t.Fatalf("Can(read): unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("Can(read) = %v, want %v", got, tt.want)
			}

			ch, err := az.Channel(ctx, tt.channelID)
			if err != nil {
				t.Fatalf("Channel: unexpected error %v", err)
			}
			legacy, err := az.canReadChannel(ctx, ch, tt.userID)
			if err != nil {
				t.Fatalf("canReadChannel: unexpected error %v", err)
			}
			if legacy != tt.want {
				t.Errorf("canReadChannel = %v, want %v", legacy, tt.want)
			}
		})
	}

	t.Run("nil channel is not readable", func(t *testing.T) {
		// Callers that pass the result of Channel() straight through must get
		// false rather than a nil dereference.
		got, err := az.canReadChannel(t.Context(), nil, w.owner)
		if err != nil {
			t.Fatalf("canReadChannel(nil): unexpected error %v", err)
		}
		if got {
			t.Error("canReadChannel(nil) = true, want false")
		}
	})
}

// TestReadableChannelIDs pins the predicate KeysFor's channel arm is built
// from. It is unexported now — the only caller is KeysFor — but it is still the
// thing every search filter ultimately narrows on, so it keeps its own table.
func TestReadableChannelIDs(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name        string
		workspaceID string
		userID      string
		want        []string
	}{
		{
			// Every public channel, archived included, plus the private one
			// they belong to. Not the dm they are not in.
			name:        "member sees public channels plus their own private one",
			workspaceID: w.wsA,
			userID:      w.member,
			want:        []string{w.chPublic, w.chPublicEmpty, w.chArchived, w.chPrivate},
		},
		{
			name:        "guest sees only the public channels",
			workspaceID: w.wsA,
			userID:      w.guest,
			want:        []string{w.chPublic, w.chPublicEmpty, w.chArchived},
		},
		{
			name:        "owner sees the dm they participate in",
			workspaceID: w.wsA,
			userID:      w.owner,
			want:        []string{w.chPublic, w.chPublicEmpty, w.chArchived, w.chDM},
		},
		{
			name:        "a non-member of the workspace sees nothing",
			workspaceID: w.wsA,
			userID:      w.stranger,
			want:        nil,
		},
		{
			name:        "wsA owner sees nothing in wsB",
			workspaceID: w.wsB,
			userID:      w.owner,
			want:        nil,
		},
		{
			name:        "missing workspace",
			workspaceID: missingID,
			userID:      w.owner,
			want:        nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.readableChannelIDs(t.Context(), tt.workspaceID, tt.userID)
			if err != nil {
				t.Fatalf("readableChannelIDs: unexpected error %v", err)
			}
			// Search turns this into a Meilisearch filter. If "no readable
			// channels" came back as nil and the caller treated nil as "no
			// filter", the query would run unconstrained and return every
			// workspace's messages — so the empty case must be a non-nil,
			// zero-length slice that a caller can distinguish.
			if got == nil {
				t.Fatal("readableChannelIDs returned nil; want an empty slice")
			}
			if !sameSet(got, tt.want) {
				t.Errorf("readableChannelIDs = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("private channels of other members are never listed", func(t *testing.T) {
		got, err := az.readableChannelIDs(t.Context(), w.wsA, w.guest)
		if err != nil {
			t.Fatalf("readableChannelIDs: unexpected error %v", err)
		}
		for _, id := range got {
			if id == w.chPrivate || id == w.chDM {
				t.Errorf("guest was offered channel %s, which they do not belong to", id)
			}
		}
	})
}

// --- blocks ------------------------------------------------------------------

func TestIsBlocked(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		// Only one row exists (blocker -> blocked); both directions must answer
		// true, or the blocked party can still route content back.
		{"blocker sees the block", w.blocker, w.blocked, true},
		{"blocked party sees the block too", w.blocked, w.blocker, true},

		{"unrelated pair", w.owner, w.member, false},
		{"self is never blocked", w.blocker, w.blocker, false},
		{"unknown user", w.blocker, missingID, false},
		{"empty first id", "", w.blocked, false},
		{"empty second id", w.blocker, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.IsBlocked(t.Context(), tt.a, tt.b)
			if err != nil {
				t.Fatalf("IsBlocked: unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("IsBlocked = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBlockedUserIDs(t *testing.T) {
	az, w := seed(t)

	tests := []struct {
		name   string
		userID string
		want   []string
	}{
		{"the blocker sees who they blocked", w.blocker, []string{w.blocked}},
		{"the blocked party sees who blocked them", w.blocked, []string{w.blocker}},
		{"an unrelated user has an empty set", w.owner, nil},
		{"an unknown user has an empty set", missingID, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := az.BlockedUserIDs(t.Context(), tt.userID)
			if err != nil {
				t.Fatalf("BlockedUserIDs: unexpected error %v", err)
			}
			// Notification fan-out indexes this map directly; a nil map would
			// read fine but tells the caller nothing about "no blocks", so the
			// contract is an allocated map.
			if got == nil {
				t.Fatal("BlockedUserIDs returned nil; want an allocated map")
			}
			if got[tt.userID] {
				t.Error("a user appeared in their own blocked set")
			}
			ids := make([]string, 0, len(got))
			for id := range got {
				ids = append(ids, id)
			}
			if !sameSet(ids, tt.want) {
				t.Errorf("BlockedUserIDs = %v, want %v", ids, tt.want)
			}
		})
	}
}

// sameSet compares two id collections irrespective of order. nil and empty are
// the same set here; the nil-vs-empty contract is asserted separately, because
// that distinction matters to the caller and order does not.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
