package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/electric"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The demonstration's own routes, and the only hand-written HTTP in this
// example.
//
// They are here rather than in the schema because none of them is about a
// table. The outbox is a ring buffer in this process's memory — it has no rows,
// no tenant column and nothing to filter; the tour is a question about how the
// binary was started; and the sync switch operates a container. A resource for
// any of them would be a resource that lies about what it is, and rig would
// generate a client, a filter grammar and an OpenAPI entry for something that
// vanishes on restart.
//
// `/_demo/` is a prefix nothing else claims: rig owns `/_rig/`, the API owns
// `/api/v1`, and the front end owns everything left over.
const demoPrefix = "/_demo/"

// registerDemo mounts them. getClaims is the same lookup every generated
// handler uses, so a signed-out browser gets a 401 here for the same reason it
// gets one from the board.
func registerDemo(mux *http.ServeMux, mail *outbox.Box, page *observe.Page,
	getClaims func(*http.Request) (tenancy.Claims, error),
	senders map[notify.Channel]notify.Sender,
	proxy *electric.Proxy, upstream string,
) {
	sync := newSyncSwitch(proxy, upstream)

	// What the tour can offer, so the front end can leave out a link that would
	// only 404. The monitoring page does not listen without a password in the
	// environment, and that is the ordinary case on a laptop.
	//
	// A URL and not a boolean, because the page is on a listener of its own now
	// and a relative href no longer reaches it. That is also why this is the
	// only place in the example that builds one: which port and which interface
	// come from rig.yaml, and the front end should not be guessing at either.
	//
	// Signed in, like the outbox below, and for a reason worth saying out loud:
	// rig gives that page no port at all rather than one that answers 401, so
	// that a scan cannot learn there is a page there. An anonymous endpoint
	// handing back its address would give away exactly the fact rig went out of
	// its way not to leak — more of it, now that the answer is where rather
	// than whether. Nothing is lost: the nav that reads this is only rendered
	// for a session anyway.
	mux.HandleFunc("GET "+demoPrefix+"tour", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := tenantOf(r, getClaims); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "sign in first"})
			return
		}
		// Which channels have a sender, because the preferences screen is a
		// screen about channels and one of them cannot work here. A channel
		// with nothing registered has no delivery rows written for it at all —
		// the right answer, and an invisible one without this.
		channels := make([]string, 0, len(senders))
		for _, c := range notify.Channels() {
			if _, ok := senders[c]; ok {
				channels = append(channels, string(c))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"monitor": monitorURL(page),
			"outbox":  mail != nil,
			// Whether this build can take the sync service down. The pill that
			// reads this is a button, and a button that answers 404 is worse
			// than no button.
			"sync":     sync != nil,
			"channels": channels,
		})
	})

	registerSyncSwitch(mux, sync, getClaims)

	if mail == nil {
		return
	}

	// The mail nobody sent. Signed in, and scoped to the caller's tenant where
	// the message knows one — see outbox.Box.For for what that does not cover
	// and why a real application has no such screen at all.
	mux.HandleFunc("GET "+demoPrefix+"outbox", func(w http.ResponseWriter, r *http.Request) {
		tenantID, ok := tenantOf(r, getClaims)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "sign in first"})
			return
		}
		messages := mail.For(tenantID)
		if messages == nil {
			messages = []outbox.Message{}
		}
		writeJSON(w, http.StatusOK, messages)
	})
}

// SyncSwitchEnv names the container the sync service runs in, and mounting the
// switch at all is conditional on it being set.
//
// This is the same gate rig puts on its monitoring page, for a stronger version
// of the same reason. A route that runs `docker stop` is a route no deployment
// may have, so the answer to "is it there?" has to default to no — and it has
// to be a route that *does not exist* rather than one that refuses, because a
// route answering 403 still tells a scanner what this process can reach.
//
// `make demo` sets it to the name `rig db up` gave the container. It is not in
// rig.yaml, which is checked in, for the same reason RIG_MONITOR_PASSWORD is
// not: a switch like this is a fact about one laptop.
const SyncSwitchEnv = "RIG_DEMO_SYNC_CONTAINER"

// syncSwitch stops and starts the sync service, and says what the proxy in
// front of it currently believes.
//
// It shells out to the container engine rather than using rig's own
// internal/dockerdb, which is not importable from an example — and would drag
// the CLI's dependencies into a module that only serves a board. Three
// subcommands is not enough code to be worth that.
type syncSwitch struct {
	// engine is an absolute path, resolved once: `docker` then `podman`, the
	// order rig's own container code tries them in.
	engine    string
	container string
	proxy     *electric.Proxy
	// upstream is the host port this process forwards shapes to, taken from the
	// same URL the proxy was built with. Kept so the state can notice the
	// container coming back somewhere else — see syncState.Moved.
	upstream string
}

// newSyncSwitch is nil when this build cannot offer the switch, which is every
// build but a demonstration on a machine with a container engine.
func newSyncSwitch(proxy *electric.Proxy, upstream string) *syncSwitch {
	name := strings.TrimSpace(os.Getenv(SyncSwitchEnv))
	if name == "" || proxy == nil {
		return nil
	}
	for _, bin := range []string{"docker", "podman"} {
		if engine, err := exec.LookPath(bin); err == nil {
			return &syncSwitch{
				engine:    engine,
				container: name,
				proxy:     proxy,
				upstream:  portOf(upstream),
			}
		}
	}
	return nil
}

// portOf is the port in a URL, and "" for one that does not parse — which is
// not worth refusing to start over, since the only thing it costs is the
// warning in syncState.Moved.
func portOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if _, port, err := net.SplitHostPort(u.Host); err == nil {
		return port
	}
	return ""
}

// syncState is what the pill in the header renders, and it reports several
// separate facts rather than one "is sync up" boolean on purpose.
//
// Container is the truth about the process. Reachable is what this proxy's
// circuit breaker last learned, which lags it in both directions and is the
// interesting part: stopping the container leaves Reachable true until enough
// requests in a row have failed to open the circuit, and starting it leaves
// Reachable false until the cooldown lets one request through to find out.
// Showing them side by side is the only way that mechanism is visible from a
// browser.
type syncState struct {
	// Container is "running", "stopped", or "missing" for one that was never
	// created — which is what `make examples` sees, since it brings up a
	// database and no sync service.
	Container string `json:"container"`
	Reachable bool   `json:"reachable"`

	// Upstream is the port this process forwards shapes to, and Published is
	// where the container actually answers — empty while it is not running.
	Upstream  string `json:"upstream"`
	Published string `json:"published"`

	// Moved says those two disagree, which is a dead end and the one failure
	// this switch can cause rather than demonstrate.
	//
	// A container published on a port the kernel chose gets a *different* one
	// when it is started again, and rig asks the kernel to choose whenever
	// RIG_DB_ISOLATE is set — which is every checkout of rig itself, because
	// its own Makefile sets it so two clones cannot adopt each other's
	// containers. The proxy's URL is fixed when the process starts, so a
	// container that came back elsewhere is a sync service that is running,
	// healthy, and permanently unreachable from here. Restarting the server is
	// the whole fix; noticing is the hard part, which is why this field exists.
	Moved bool `json:"moved"`
}

// registerSyncSwitch mounts the three routes, and mounts nothing when the
// switch is nil.
func registerSyncSwitch(mux *http.ServeMux, sw *syncSwitch, getClaims func(*http.Request) (tenancy.Claims, error)) {
	if sw == nil {
		return
	}

	// Signed in, like the tour and for the same reason: what this process can
	// reach is not a fact to hand an anonymous caller.
	guard := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if _, ok := tenantOf(r, getClaims); !ok {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "sign in first"})
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET "+demoPrefix+"sync", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sw.state(r.Context()))
	}))

	// Both answer with the state afterwards, so the front end needs no second
	// request to know what it did. The engine's own subcommand is the route's
	// last segment, which is why one loop covers both.
	for _, verb := range []string{"stop", "start"} {
		mux.HandleFunc("POST "+demoPrefix+"sync/"+verb, guard(func(w http.ResponseWriter, r *http.Request) {
			// Detached from the request on purpose. The engine does the work
			// either way, so a browser that navigates away mid-stop would
			// otherwise leave this handler reporting a failure for something
			// that succeeded.
			ctx := context.WithoutCancel(r.Context())
			if out, err := sw.run(ctx, verb, sw.container); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"message": strings.TrimSpace(out),
				})
				return
			}
			writeJSON(w, http.StatusOK, sw.state(ctx))
		}))
	}
}

// inspectFormat asks for both facts in one call: whether the container is
// running, and which host port it publishes the sync service on. A stopped
// container has the mapping but no port, and both `with`s are what keep that
// from being a template error rather than an empty second field.
const inspectFormat = `{{.State.Running}} {{with index .NetworkSettings.Ports "3000/tcp"}}{{with index . 0}}{{.HostPort}}{{end}}{{end}}`

// state asks the engine about the container and the proxy about itself.
func (s *syncSwitch) state(ctx context.Context) syncState {
	st := syncState{Container: "missing", Reachable: s.proxy.SyncReachable(), Upstream: s.upstream}
	// A container that does not exist is an error here, not a false — which is
	// the answer `make examples` gets, and not a failure.
	out, err := s.run(ctx, "inspect", "--format", inspectFormat, s.container)
	if err != nil {
		return st
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return st
	}
	if fields[0] == "true" {
		st.Container = "running"
	} else {
		st.Container = "stopped"
	}
	if len(fields) > 1 {
		st.Published = fields[1]
	}
	st.Moved = st.Published != "" && st.Upstream != "" && st.Published != st.Upstream
	return st
}

// run is one engine command, with a ceiling. `stop` sends SIGTERM and waits ten
// seconds for the process to go before killing it, so the budget has to be
// larger than that or a graceful stop reads as a failed one.
func (s *syncSwitch) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.engine, args...).CombinedOutput()
	return string(out), err
}

// monitorURL is where a browser reaches the monitoring page, and empty when
// there is none to reach.
//
// It has to be absolute: the page listens on a port of its own, so it is a
// different origin from the one this document was served from. The scheme is
// http because that is what a listener bound to loopback is — a demo running on
// the machine you are sitting at. A deployment that put the page behind TLS put
// something in front of it, and that something knows its own address better
// than this does.
//
// A wildcard bind is rewritten to localhost. ":9084" is a valid thing to listen
// on and not a valid thing to put in an href, and the browser asking is on this
// machine by construction — it is reading a page this process served.
func monitorURL(page *observe.Page) string {
	addr := page.Addr()
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + page.BasePath() + "/"
}

// tenantOf is the caller's tenant, and false for anybody these routes should
// not answer at all.
func tenantOf(r *http.Request, getClaims func(*http.Request) (tenancy.Claims, error)) (uuid.UUID, bool) {
	claims, err := getClaims(r)
	if err != nil || claims.TenantID == uuid.Nil {
		return uuid.Nil, false
	}
	return claims.TenantID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Nothing here is cacheable: one is a credential and the others are facts
	// about this process.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
