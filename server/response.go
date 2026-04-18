package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strconv"
)

// ErrorBody is the standard error response envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error code and message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PagedResponse wraps a list response with cursor pagination.
type PagedResponse[T any] struct {
	Data       []T    `json:"data"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorBody{
		Error: ErrorDetail{Code: code, Message: message},
	})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// encodeCursor encodes an int64 ID as a base64 cursor string.
func encodeCursor(id int64) string {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(id))
	return base64.URLEncoding.EncodeToString(b)
}

// decodeCursor decodes a base64 cursor string to an int64 ID. Returns 0 for empty cursor.
func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	b, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	if len(b) != 8 {
		return 0, nil
	}
	return int64(binary.BigEndian.Uint64(b)), nil
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
