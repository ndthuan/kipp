package api

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// listJSON is a list: the rows, and where the next page of them begins, null
// when there is none.
type listJSON[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// writeList answers a list whose next page begins at the cursor next, none
// when it is empty.
func writeList[T any](w http.ResponseWriter, items []T, next string) {
	out := listJSON[T]{Items: items}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// trim is the page of n rows out of found, which the store was asked for
// one row longer than a page: more says that row was there, so that a page
// after this one exists.
func trim[T any](found []T, n int) (page []T, more bool) {
	if len(found) <= n {
		return found, false
	}
	return found[:n], true
}

// cursorFor is the opaque cursor of the page that follows a row of the list
// kind: the position of that row, in the list's own order.
func cursorFor(kind string, position ...int64) string {
	parts := make([]string, 0, 1+len(position))
	parts = append(parts, kind)
	for _, p := range position {
		parts = append(parts, strconv.FormatInt(p, 10))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, ".")))
}

// cursorOf reads the ?cursor= of a request for the list kind, whose cursors
// hold n numbers: nil when it has none. One that this server did not make
// for this list is ErrBadInput.
func cursorOf(r *http.Request, kind string, n int) ([]int64, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, nil
	}
	bad := fmt.Errorf("%w: cursor is not one this server gave for this list", ErrBadInput)
	text, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, bad
	}
	parts := strings.Split(string(text), ".")
	if len(parts) != n+1 || parts[0] != kind {
		return nil, bad
	}
	out := make([]int64, n)
	for i, p := range parts[1:] {
		if out[i], err = strconv.ParseInt(p, 10, 64); err != nil {
			return nil, bad
		}
	}
	return out, nil
}

// firstOf is the one number of a cursor that holds one, 0 for no cursor.
func firstOf(at []int64) int64 {
	if len(at) == 0 {
		return 0
	}
	return at[0]
}

// limitOf reads the ?limit= of a request, 0 when it has none: a whole number
// of 1 or more, and never read as another. It is the rows asked for, which
// rows keeps to what a page may hold.
func limitOf(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: limit is a whole number, 1 or more", ErrBadInput)
	}
	return n, nil
}

// pageArgs reads how many rows a request for the list kind asks for and
// where its page begins, by ?limit= and ?cursor=, with n numbers in a
// cursor.
func pageArgs(r *http.Request, kind string, n int) (limit int, position []int64, err error) {
	if limit, err = limitOf(r); err != nil {
		return 0, nil, err
	}
	position, err = cursorOf(r, kind, n)
	return limit, position, err
}

// flagOf reads a true-or-false query value; anything else is refused.
func flagOf(r *http.Request, name string) (bool, error) {
	switch r.URL.Query().Get(name) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%w: %s is true or false", ErrBadInput, name)
}

// offsetOf reads the ?offset= of a request for a log: a byte offset, 0 when
// there is none. One that is not a whole number of 0 or more is refused.
func offsetOf(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("offset")
	n, err := strconv.ParseInt(raw, 10, 64)
	switch {
	case raw == "":
		return 0, nil
	case err != nil || n < 0:
		return 0, fmt.Errorf("%w: offset is a whole number, 0 or more", ErrBadInput)
	}
	return n, nil
}
