//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

type issueState struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

type issue struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Number    int     `json:"number"`
	Key       string  `json:"key"`
	Title     string  `json:"title"`
	StateID   string  `json:"state_id"`
	Assignee  *string `json:"assignee_id"`
	Rank      string  `json:"rank"`
}

type issuePage struct {
	Issues  []issue           `json:"issues"`
	Next    map[string]string `json:"next"`
	HasMore bool              `json:"has_more"`
}

func (h *harness) createProject(t *testing.T, token, ws, name, key string) project {
	t.Helper()
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/projects", token,
		map[string]string{"name": name, "key": key})
	var p project
	decodeInto(t, resp.Data, &p)
	return p
}

func (h *harness) states(t *testing.T, token, projectID string) []issueState {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/projects/"+projectID+"/states", token, nil)
	var out []issueState
	decodeInto(t, resp.Data, &out)
	return out
}

func (h *harness) createIssue(t *testing.T, token, projectID, title string) issue {
	t.Helper()
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/projects/"+projectID+"/issues", token, map[string]string{"title": title})
	var i issue
	decodeInto(t, resp.Data, &i)
	return i
}

func (h *harness) issues(t *testing.T, token, projectID string) []issue {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/projects/"+projectID+"/issues", token, nil)
	var page issuePage
	decodeInto(t, resp.Data, &page)
	return page.Issues
}

// A project arrives COMPLETE: states, a counter, an ACL row and the grant that
// makes it visible. A project with no states is a project nothing can be created
// in, so "create a project" would otherwise be an incomplete action.
func TestProjectIsCreatedComplete(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	p := h.createProject(t, admin, ws, "Platform", uniqueKey(t))

	states := h.states(t, admin, p.ID)
	if len(states) != 5 {
		t.Fatalf("a new project has %d states, want the five defaults: %+v", len(states), states)
	}
	categories := map[string]bool{}
	for _, s := range states {
		categories[s.Category] = true
	}
	for _, want := range []string{"backlog", "unstarted", "started", "completed", "cancelled"} {
		if !categories[want] {
			t.Errorf("no default state in category %q; reports and workflow triggers reason "+
				"about categories, not names", want)
		}
	}

	// It appears in its own listing IMMEDIATELY — the ACL row is written in the
	// same transaction, not by the hourly drift job.
	resp := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workspaces/"+ws+"/projects", admin, nil)
	var projects []project
	decodeInto(t, resp.Data, &projects)
	found := false
	for _, x := range projects {
		if x.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a project created one request ago is not in its own workspace's listing")
	}

	// A member sees it too: the workspace grant is what makes a project visible.
	member := h.newUser(t, admin, ws, "issue-member")
	resp = h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workspaces/"+ws+"/projects", member.token, nil)
	decodeInto(t, resp.Data, &projects)
	found = false
	for _, x := range projects {
		if x.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Error("a workspace member cannot see a project in their own workspace")
	}
}

// PROJ-14 is what people paste into chat, so the number must not skip.
//
// A SEQUENCE burns its value on a rollback, and the gap is indistinguishable
// from a deleted issue — "where did PROJ-13 go" is a support question with no
// answer. A row counter is transactional.
func TestIssueNumbersAreSequentialAndGapless(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	key := uniqueKey(t)
	p := h.createProject(t, admin, ws, "Numbering", key)

	for n := 1; n <= 3; n++ {
		i := h.createIssue(t, admin, p.ID, fmt.Sprintf("issue %d", n))
		if i.Number != n {
			t.Fatalf("issue %d got number %d", n, i.Number)
		}
		if want := fmt.Sprintf("%s-%d", key, n); i.Key != want {
			t.Fatalf("issue key = %q, want %q", i.Key, want)
		}
	}

	// A REFUSED create must not burn a number. An empty title is refused after
	// the transaction would have opened, which is exactly the case a sequence
	// gets wrong.
	h.denied(t, http.StatusBadRequest, http.MethodPost,
		"/api/v1/projects/"+p.ID+"/issues", admin, map[string]string{"title": "   "})

	if next := h.createIssue(t, admin, p.ID, "after a refusal"); next.Number != 4 {
		t.Errorf("after a refused create the next number is %d, want 4 — a gap is "+
			"indistinguishable from a deleted issue", next.Number)
	}
}

// An issue in project A must not hold project B's state.
//
// A plain REFERENCES issue_states(id) permits it, does not error, and the issue
// then vanishes from its own board — the state is absent from A's column list,
// so nothing renders it.
func TestIssueCannotHoldAnotherProjectsState(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	a := h.createProject(t, admin, ws, "Alpha", uniqueKey(t))
	b := h.createProject(t, admin, ws, "Beta", uniqueKey(t))
	foreign := h.states(t, admin, b.ID)[0]

	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/projects/"+a.ID+"/issues", admin,
		map[string]string{"title": "wrong state", "state_id": foreign.ID})

	// ...and a MOVE into another project's state is refused too, which is the
	// path a board drag takes.
	i := h.createIssue(t, admin, a.ID, "legit")
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/issues/"+i.ID+"/move", admin,
		map[string]string{"state_id": foreign.ID})
}

// A drag names its NEIGHBOURS, never a rank. The client knows what it dropped
// between; it does not know the rank algebra, and a client-computed rank would
// be a second implementation of it against a board that may have moved.
func TestIssueMoveReordersByNeighbours(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	p := h.createProject(t, admin, ws, "Ordering", uniqueKey(t))

	// New issues go to the TOP, so creating a, b, c leaves c, b, a.
	a := h.createIssue(t, admin, p.ID, "a")
	b := h.createIssue(t, admin, p.ID, "b")
	c := h.createIssue(t, admin, p.ID, "c")

	order := func() []string {
		out := []string{}
		for _, i := range h.issues(t, admin, p.ID) {
			out = append(out, i.Title)
		}
		return out
	}
	if got := order(); len(got) != 3 || got[0] != "c" {
		t.Fatalf("initial order = %v, want the newest first", got)
	}

	// Drag a to the very top: no lower neighbour, upper neighbour is c.
	h.req(t, http.StatusOK, http.MethodPost, "/api/v1/issues/"+a.ID+"/move", admin,
		map[string]string{"before_id": c.ID})
	if got := order(); got[0] != "a" {
		t.Fatalf("after moving a to the top the order is %v", got)
	}

	// Drag c between a and b.
	h.req(t, http.StatusOK, http.MethodPost, "/api/v1/issues/"+c.ID+"/move", admin,
		map[string]string{"after_id": a.ID, "before_id": b.ID})
	if got := order(); len(got) != 3 || got[0] != "a" || got[1] != "c" || got[2] != "b" {
		t.Fatalf("after the second move the order is %v, want [a c b]", got)
	}

	// Repeatedly dropping into the SAME gap must keep working. This is where a
	// float representation dies silently after ~50 inserts and two cards start
	// comparing equal.
	for n := range 60 {
		extra := h.createIssue(t, admin, p.ID, fmt.Sprintf("squeeze-%d", n))
		h.req(t, http.StatusOK, http.MethodPost, "/api/v1/issues/"+extra.ID+"/move", admin,
			map[string]string{"after_id": a.ID, "before_id": c.ID})
	}
	got := h.issues(t, admin, p.ID)
	for i := 1; i < len(got); i++ {
		if got[i-1].Rank >= got[i].Rank {
			t.Fatalf("ranks are not strictly increasing at %d: %q >= %q",
				i, got[i-1].Rank, got[i].Rank)
		}
	}
	if got[0].Title != "a" {
		t.Errorf("after 60 insertions into one gap the head moved: %q", got[0].Title)
	}
}

// Moving into a completed state stamps completed_at; moving back clears it.
func TestIssueCompletionFollowsTheStateCategory(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	p := h.createProject(t, admin, ws, "Completion", uniqueKey(t))

	var done, todo string
	for _, s := range h.states(t, admin, p.ID) {
		switch s.Category {
		case "completed":
			done = s.ID
		case "unstarted":
			todo = s.ID
		}
	}
	i := h.createIssue(t, admin, p.ID, "finish me")

	type completion struct {
		StateID     string  `json:"state_id"`
		CompletedAt *string `json:"completed_at"`
	}
	// A FRESH struct per decode. json.Unmarshal does not clear fields the
	// payload omits, so reusing one makes an absent field read as its previous
	// value — which is how the first version of this test passed the assertion
	// it was written to make.
	move := func(stateID string) completion {
		t.Helper()
		resp := h.req(t, http.StatusOK, http.MethodPost, "/api/v1/issues/"+i.ID+"/move", admin,
			map[string]string{"state_id": stateID})
		var out completion
		decodeInto(t, resp.Data, &out)
		return out
	}

	if got := move(done); got.CompletedAt == nil {
		t.Error("moving into a completed state did not stamp completed_at; every report " +
			"that asks 'when was this done' reads that column")
	}
	if got := move(todo); got.CompletedAt != nil {
		t.Errorf("reopening an issue left completed_at at %v", *got.CompletedAt)
	}
}

// Tenancy, written as the attack.
func TestIssuesAreNotReachableFromAnotherTenant(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	p := h.createProject(t, admin, ws, "Confidential", uniqueKey(t))
	i := h.createIssue(t, admin, p.ID, "the acquisition")

	other := h.newTenant(t, "issue-outsider")
	for _, path := range []string{
		"/api/v1/projects/" + p.ID,
		"/api/v1/projects/" + p.ID + "/issues",
		"/api/v1/projects/" + p.ID + "/states",
		"/api/v1/issues/" + i.ID,
	} {
		h.denied(t, http.StatusNotFound, http.MethodGet, path, other.token, nil)
	}
	h.denied(t, http.StatusNotFound, http.MethodPost,
		"/api/v1/projects/"+p.ID+"/issues", other.token, map[string]string{"title": "trojan"})
	h.denied(t, http.StatusNotFound, http.MethodPost,
		"/api/v1/issues/"+i.ID+"/move", other.token, map[string]string{})

	// Nor by listing their own workspace's projects.
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/workspaces/"+other.workspaceID+"/projects", other.token, nil)
	var projects []project
	decodeInto(t, resp.Data, &projects)
	for _, x := range projects {
		if x.ID == p.ID {
			t.Fatal("another tenant's project appears in their listing")
		}
	}
}

// An issue is commentable the moment it exists, because it is an acl_object and
// the comment surface keys on that. Nothing in internal/comment knows what an
// issue is.
func TestIssuesAreCommentableWithNoChangeToTheCommentSurface(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	p := h.createProject(t, admin, ws, "Discussion", uniqueKey(t))
	i := h.createIssue(t, admin, p.ID, "needs discussion")

	c := h.comment(t, admin, "issue", i.ID, "I looked at this", "")
	if c.ObjectType != "issue" || c.ObjectID != i.ID {
		t.Fatalf("the comment names %s:%s", c.ObjectType, c.ObjectID)
	}
	if got := h.comments(t, admin, "issue", i.ID); len(got) != 1 {
		t.Fatalf("thread = %d, want 1", len(got))
	}

	// And another tenant cannot read the thread, because they cannot read the
	// issue — which is the entire authorization rule.
	other := h.newTenant(t, "comment-issue-outsider")
	h.denied(t, http.StatusNotFound, http.MethodGet,
		"/api/v1/comments/issue/"+i.ID, other.token, nil)
}

// uniqueKey mints a project key that cannot collide with another test's.
//
// The suite shares one workspace and one database across the whole package, and
// project keys are unique per workspace — so a fixed key would make the SECOND
// test that ran fail, in an order-dependent way.
//
// Uppercase letters and digits only, 2-10 characters, matching the column's
// CHECK.
var keySeq atomic.Int64

func uniqueKey(t *testing.T) string {
	t.Helper()
	n := keySeq.Add(1)
	return fmt.Sprintf("T%d%d", time.Now().UnixNano()%100000, n)
}
