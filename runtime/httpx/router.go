package httpx

import "net/http"

// Router is where a route is registered: the two methods of
// [net/http.ServeMux] that mounting one needs, and nothing else.
//
// It is an interface rather than the mux itself so that a generated server can
// put something between the pattern and the handler — opening a span named by
// the route is the one thing that does so today — and have it cover the routes
// rig mounts as well as the ones it generates. A *http.ServeMux satisfies it,
// so a caller that has nothing to interpose passes the mux.
//
// It lives here because the packages that mount rig's own routes — authhttp,
// oauth, notifyhttp, presencehttp, apidoc — already share this package's error
// writer, and an interface declared in one of them would be a dependency the
// others have no other reason for.
type Router interface {
	// Handle registers handler for pattern.
	Handle(pattern string, handler http.Handler)

	// HandleFunc registers handler for pattern.
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}
