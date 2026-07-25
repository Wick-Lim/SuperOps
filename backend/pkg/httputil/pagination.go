package httputil

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DefaultPageSize = 50
const MaxPageSize = 100

// Cursor is a keyset-pagination position. Ordering on created_at alone is not
// total — the scheduled-message batch promotion assigns one transaction
// timestamp to a whole batch — so a strictly-less-than filter on the timestamp
// silently drops every row but one at a page boundary. ID is the tiebreaker
// that makes (CreatedAt, ID) a total order.
//
// A zero Cursor (IsZero() == true) means "first page".
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

func (c Cursor) IsZero() bool { return c.CreatedAt.IsZero() }

type PaginationParams struct {
	Cursor Cursor
	Limit  int
}

// ErrBadCursor is returned by ParsePagination for a malformed cursor or an
// out-of-range limit. Previously both were swallowed, so a forged cursor
// silently returned page 1 and an oversized limit was silently clamped —
// neither of which a caller could detect.
var ErrBadCursor = NewBadRequest("invalid pagination parameter")

func ParsePagination(r *http.Request) (PaginationParams, error) {
	p := PaginationParams{Limit: DefaultPageSize}

	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := DecodeCursor(raw)
		if err != nil {
			return p, ErrBadCursor
		}
		p.Cursor = c
	}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		l, err := strconv.Atoi(raw)
		if err != nil || l <= 0 || l > MaxPageSize {
			return p, NewBadRequest(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
		}
		p.Limit = l
	}

	return p, nil
}

// EncodeCursor renders a keyset position as an opaque base64url token.
// Wire format is "<RFC3339Nano>|<id>".
func EncodeCursor(t time.Time, id string) string {
	return base64.URLEncoding.EncodeToString([]byte(t.Format(time.RFC3339Nano) + "|" + id))
}

// DecodeCursor parses a token produced by EncodeCursor. Tokens issued by the
// previous timestamp-only format are still accepted so that clients holding a
// cursor across the upgrade keep paginating instead of erroring; they simply
// get no tiebreaker until their next page.
func DecodeCursor(raw string) (Cursor, error) {
	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, fmt.Errorf("decode cursor: %w", err)
	}

	ts, id, _ := strings.Cut(string(decoded), "|")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, fmt.Errorf("parse cursor timestamp: %w", err)
	}
	return Cursor{CreatedAt: t, ID: id}, nil
}

func PathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}

func QueryParam(r *http.Request, name string) string {
	return r.URL.Query().Get(name)
}

func RequirePathParam(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	if v == "" {
		return "", fmt.Errorf("missing path parameter: %s", name)
	}
	return v, nil
}
