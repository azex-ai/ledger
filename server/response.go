package server

import (
	"net/http"
	"strconv"
)

// PagedResponse is a list response (api-contract §6): the list field is
// named "list". NextCursor is a pointer so the wire value is a literal JSON
// null when there is no next page -- api-contract.md §6 requires
// "next_cursor: null" as an explicit, type-stable sentinel a consumer can
// compare against, not an omitted key (undefined in JS) or an empty string
// (a third, undocumented "exhausted" spelling). Handlers backed by a real
// cursor query set NextCursor via cursorPtr(nextCursor); handlers that
// return an unpaginated full list simply never assign it, which serializes
// to the same "next_cursor": null -- true in that case too, since there is
// in fact no next page, ever.
type PagedResponse[T any] struct {
	List       []T     `json:"list"`
	NextCursor *string `json:"next_cursor"`
}

// cursorPtr converts a store-layer cursor string ("" meaning exhausted) into
// the wire pointer PagedResponse.NextCursor expects: nil (-> JSON null) when
// exhausted, a pointer to the cursor otherwise.
func cursorPtr(next string) *string {
	if next == "" {
		return nil
	}
	return &next
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
