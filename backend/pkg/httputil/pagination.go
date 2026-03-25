package httputil

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const DefaultPageSize = 50
const MaxPageSize = 100

type PaginationParams struct {
	Cursor time.Time
	Limit  int
}

func ParsePagination(r *http.Request) PaginationParams {
	p := PaginationParams{
		Limit: DefaultPageSize,
	}

	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		if decoded, err := base64.URLEncoding.DecodeString(cursor); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, string(decoded)); err == nil {
				p.Cursor = t
			}
		}
	}

	if limit := r.URL.Query().Get("limit"); limit != "" {
		if l, err := strconv.Atoi(limit); err == nil && l > 0 && l <= MaxPageSize {
			p.Limit = l
		}
	}

	return p
}

func EncodeCursor(t time.Time) string {
	return base64.URLEncoding.EncodeToString([]byte(t.Format(time.RFC3339Nano)))
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
