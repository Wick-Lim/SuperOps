package authz

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// world is the fixture graph every test in this package asserts against. It is
// built exactly once: the assertions are all reads, so there is nothing to
// isolate, and re-migrating or re-seeding per test would dominate the runtime.
//
// The shape is deliberately the one that keeps going wrong in production:
// two unrelated tenants (wsA, wsB) that share no membership at all, plus a
// public channel in wsA that a wsB-only account must not be able to read.
type world struct {
	// wsA members
	owner  string // owner of wsA
	admin  string // admin of wsA, member of no channel
	member string // plain member of wsA, member of chPrivate
	guest  string // guest of wsA, member of no channel

	// Other tenants
	stranger   string // sole owner of wsB; shares nothing with wsA
	multiAdmin string // admin of wsA and of wsB

	// Block fixtures — both are wsA members so a block, not a missing
	// membership, is what any negative answer is caused by.
	blocker string
	blocked string

	wsA string
	wsB string

	chPublic      string // public, member: owner (as channel admin)
	chPublicEmpty string // public, no members at all
	chPrivate     string // private, member: member
	chDM          string // dm, members: owner, admin
	chArchived    string // public + archived
	chOtherTenant string // public channel inside wsB

	msgInPrivate string // message in chPrivate
}

var (
	worldOnce sync.Once
	fixtures  *world
	worldErr  error
)

// seed builds (once) and returns the fixture graph.
func seed(t *testing.T) (*Checker, *world) {
	t.Helper()
	pool := testDB(t)
	worldOnce.Do(func() {
		fixtures, worldErr = buildWorld(context.Background(), pool)
	})
	if worldErr != nil {
		t.Fatalf("seed fixtures: %v", worldErr)
	}
	return New(pool), fixtures
}

func buildWorld(ctx context.Context, pool *pgxpool.Pool) (*world, error) {
	b := &builder{ctx: ctx, pool: pool}
	w := &world{}

	w.owner = b.user("owner")
	w.admin = b.user("admin")
	w.member = b.user("member")
	w.guest = b.user("guest")
	w.stranger = b.user("stranger")
	w.multiAdmin = b.user("multiadmin")
	w.blocker = b.user("blocker")
	w.blocked = b.user("blocked")

	w.wsA = b.workspace("alpha", w.owner)
	b.member_(w.wsA, w.owner, RoleOwner)
	b.member_(w.wsA, w.admin, RoleAdmin)
	b.member_(w.wsA, w.member, RoleMember)
	b.member_(w.wsA, w.guest, RoleGuest)
	b.member_(w.wsA, w.multiAdmin, RoleAdmin)
	b.member_(w.wsA, w.blocker, RoleMember)
	b.member_(w.wsA, w.blocked, RoleMember)

	w.wsB = b.workspace("beta", w.stranger)
	b.member_(w.wsB, w.stranger, RoleOwner)
	b.member_(w.wsB, w.multiAdmin, RoleAdmin)

	w.chPublic = b.channel(w.wsA, "general", "public", false)
	b.channelMember(w.chPublic, w.owner, ChannelRoleAdmin)

	w.chPublicEmpty = b.channel(w.wsA, "lobby", "public", false)
	w.chPrivate = b.channel(w.wsA, "secrets", "private", false)
	b.channelMember(w.chPrivate, w.member, ChannelRoleMember)

	w.chDM = b.channel(w.wsA, "dm-1", "dm", false)
	b.channelMember(w.chDM, w.owner, ChannelRoleMember)
	b.channelMember(w.chDM, w.admin, ChannelRoleMember)

	w.chArchived = b.channel(w.wsA, "attic", "public", true)
	w.chOtherTenant = b.channel(w.wsB, "beta-general", "public", false)

	w.msgInPrivate = b.message(w.chPrivate, w.member, "classified")

	b.block(w.blocker, w.blocked)

	if b.err != nil {
		return nil, b.err
	}
	return w, nil
}

// builder accumulates the first insert error instead of returning one from
// every call, so buildWorld reads as a description of the graph.
type builder struct {
	ctx  context.Context
	pool *pgxpool.Pool
	err  error
	n    int
}

func (b *builder) scalar(query string, args ...any) string {
	if b.err != nil {
		return ""
	}
	var id string
	if err := b.pool.QueryRow(b.ctx, query, args...).Scan(&id); err != nil {
		b.err = fmt.Errorf("%s: %w", query, err)
		return ""
	}
	return id
}

func (b *builder) exec(query string, args ...any) {
	if b.err != nil {
		return
	}
	if _, err := b.pool.Exec(b.ctx, query, args...); err != nil {
		b.err = fmt.Errorf("%s: %w", query, err)
	}
}

func (b *builder) user(name string) string {
	b.n++
	return b.scalar(
		`INSERT INTO users (email, username, full_name) VALUES ($1, $2, $3) RETURNING id`,
		fmt.Sprintf("%s-%d@test.local", name, b.n), fmt.Sprintf("%s%d", name, b.n), name)
}

func (b *builder) workspace(slug, ownerID string) string {
	return b.scalar(
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1, $2, $3) RETURNING id`,
		slug, slug, ownerID)
}

// member_ has a trailing underscore because `member` is a field name used all
// over this file and a method would shadow nothing useful.
func (b *builder) member_(workspaceID, userID, role string) {
	b.exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		workspaceID, userID, role)
}

func (b *builder) channel(workspaceID, slug, chType string, archived bool) string {
	return b.scalar(
		`INSERT INTO channels (workspace_id, name, slug, type, is_archived)
		 VALUES ($1, $2, $2, $3::channel_type, $4) RETURNING id`,
		workspaceID, slug, chType, archived)
}

func (b *builder) channelMember(channelID, userID, role string) {
	b.exec(`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3)`,
		channelID, userID, role)
}

func (b *builder) message(channelID, userID, content string) string {
	return b.scalar(
		`INSERT INTO messages (channel_id, user_id, content) VALUES ($1, $2, $3) RETURNING id`,
		channelID, userID, content)
}

func (b *builder) block(blockerID, blockedID string) {
	b.exec(`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)`, blockerID, blockedID)
}

// missingID is a syntactically valid uuid that no fixture uses. The columns are
// UUID, so "not-a-uuid" would produce a Postgres error rather than the
// not-found answer these tests are about.
const missingID = "00000000-0000-4000-8000-000000000000"
