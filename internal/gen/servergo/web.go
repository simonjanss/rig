package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// hasWeb reports whether this project serves its front end somewhere other than
// this API.
//
// Independent of authentication on purpose. A project with a single-page
// application and no provider sign-in still has a front end origin, and the
// cross-origin half of this needs it — which is why `web:` is a top-level block
// rather than a key under auth.
func (e *emitter) hasWeb() bool { return e.doc.API.Web != nil }

// webFile emits what a browser front end on another origin makes knowable.
//
// A file of its own for the reason auth.gen.go and tracing.gen.go are: a project
// without the block gets no file at all, and there is nothing to explain about a
// constant that is not there.
func (e *emitter) webFile() (gen.Artifact, error) {
	w := e.doc.API.Web
	b := gobuf.New(e.cfg.Package)

	if w.OriginEnv != "" {
		b.Comment("WebOriginEnv is where the front end's origin comes from, for a " +
			"deployment whose front end differs from environment to environment. " +
			"[WebOrigin] reads it.\n\n" +
			"A constant rather than a string inside that function, so what a " +
			"deployment has to set is readable off the package — and so the refusal " +
			"for having not set it can name it.")
		b.L("const WebOriginEnv = %s", gobuf.Quote(w.OriginEnv))
		b.NL()
	}

	b.Comment("WebCallbackPath is the route on the front end that receives a " +
		"finished provider sign-in.\n\n" +
		"It is the front end's own route, so rig cannot check that it exists — " +
		"what rig does with it is resolve the redirect and scope the handoff " +
		"cookie to that one path, which is why the two have to be the same string " +
		"and why it comes from rig.yaml rather than from each end separately.")
	b.L("const WebCallbackPath = %s", gobuf.Quote(w.CallbackPath))
	b.NL()

	doc := "WebOrigin is where this API's browser front end is served.\n\n" +
		"It is a function rather than a constant for the reason [BaseURL] is, " +
		"where a project has both: the same binary serves more than one " +
		"deployment and the configuration cannot know which."
	if w.OriginEnv != "" && w.Origin == "" {
		doc += " Which is also why it can fail: " + w.OriginEnv + " is the only " +
			"thing this configuration names, so a deployment that forgot to set it " +
			"has no front end, and the error says so here rather than at the first " +
			"sign-in that tries to redirect to it."
	}
	doc += "\n\nWhat the configuration and the environment say, which is not the " +
		"whole answer: [Hooks.OAuth] carries a WebOrigin of its own and [Config] " +
		"prefers it. This is what a project that supplied none gets."

	originFunc(b, originSpec{
		Name:     "WebOrigin",
		EnvConst: "WebOriginEnv",
		Env:      w.OriginEnv,
		Literal:  w.Origin,
		Doc:      doc,
		Package:  e.cfg.Package,
		Unset: w.OriginEnv + " must name the origin this application's browser " +
			"front end is served on: a finished provider sign-in redirects there " +
			"and leaves its tokens in a cookie scoped to it, and " +
			"Hooks.OAuth.WebOrigin is the other way to supply it",
	})

	return artifact("web.gen.go", b)
}
