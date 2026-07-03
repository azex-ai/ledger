package server

import (
	"net/http"
	"strconv"
)

// PagedResponse is a cursor-paginated list response (api-contract §6): the
// list field is named "list" and next_cursor is null/omitted when exhausted.
type PagedResponse[T any] struct {
	List       []T    `json:"list"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// parsePageLimit reads the "limit" query param, defaulting to 50, capped at 200.
func parsePageLimit(r *http.Request) int32 {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 50
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return int32(n)
}

// parseIDParam parses a numeric URL path parameter.
func parseIDParam(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
