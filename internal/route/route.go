// Package route is the row of a route table that the machine-facing
// packages (webhook, runnerhub, cachesrv) export, as api.Route does for the
// JSON API, so that the API description is checked against what is served.
package route

import "net/http"

// Route is one served endpoint: a method and the full path, with the
// handler it reaches.
type Route struct {
	Method  string
	Path    string // with its prefix, {name} for a variable part
	Handler http.Handler
}

// Pattern is the row as a ServeMux pattern.
func (r Route) Pattern() string { return r.Method + " " + r.Path }

// Mount registers every row on mux.
func Mount(mux *http.ServeMux, rows []Route) {
	for _, r := range rows {
		mux.Handle(r.Pattern(), r.Handler)
	}
}
