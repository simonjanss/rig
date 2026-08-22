package openapigen

import (
	"slices"
	"strconv"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"
	"go.yaml.in/yaml/v4"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/ir"
)

// The builtin shapes, by the names the compiler injects them under.
const (
	objectError      = "Error"
	objectPagination = "Pagination"
	enumErrorCode    = "ErrorCode"
)

// securitySchemeName is the one scheme this document declares.
const securitySchemeName = "bearerAuth"

// errorCodes pairs each HTTP status with the code a client will find in the
// body of a failure carrying it.
//
// ir.Endpoint.Errors is a list of bare statuses, and its doc comment says
// generators pair each with the matching ErrorCode value — but the pairing is
// in neither the endpoint nor the enum, which carries only names and words.
// compile.ErrorCodes is where the two are written down together, and it is the
// same table the enum itself was built from, so reading it here cannot
// disagree with the document.
//
// The description is taken from the document's own enum where it has one, so a
// project that replaced the builtin keeps its own words; compile.ErrorCodes
// supplies only the status. A status the table does not name is left out, and
// a test fails rather than a response rendering with an empty description.
func (e *emitter) errorCodes() map[int]ir.EnumValue {
	words := map[string]ir.EnumValue{}
	if en := e.enum(enumErrorCode); en != nil {
		for _, v := range en.Values {
			words[v.Name] = v
		}
	}

	out := map[int]ir.EnumValue{}
	for _, c := range compile.ErrorCodes {
		v, ok := words[c.Name]
		if !ok {
			v = ir.EnumValue{Name: c.Name, Wire: c.Name, Description: c.Description}
		}
		if _, dup := out[c.Status]; !dup {
			out[c.Status] = v
		}
	}
	return out
}

// errorResponseName is the components/responses key for one status.
func (e *emitter) errorResponseName(status int) string {
	if v, ok := e.errorCodes()[status]; ok {
		return v.Name
	}
	return "Status" + strconv.Itoa(status)
}

// errorResponses builds one shared response per status any operation can
// return.
//
// This is why the IR stores bare codes: every one of these has the same body,
// and spelling the Error schema out on each of the eleven statuses of each of
// the dozen endpoints would triple the document without adding a fact.
func (e *emitter) errorResponses() *orderedmap.Map[string, *v3.Response] {
	codes := e.errorCodes()

	statuses := make([]int, 0, len(e.usedStatuses))
	for s := range e.usedStatuses {
		statuses = append(statuses, s)
	}
	slices.Sort(statuses)

	out := orderedmap.New[string, *v3.Response]()
	for _, status := range statuses {
		v, known := codes[status]
		desc := v.Description
		if desc == "" {
			desc = "The request failed."
		}
		if known {
			desc += "\n\nThe body's `code` is `" + v.Wire + "`."
		}

		resp := &v3.Response{Description: desc, Content: jsonContent(schemaRef(objectError))}
		resp.Headers = orderedmap.New[string, *v3.Header]()
		if status == 429 {
			// The only failure that carries anything beyond the body, and the
			// only one a client is expected to act on rather than report.
			for _, h := range rateLimitHeaders {
				resp.Headers.Set(h.name, &v3.Header{Reference: "#/components/headers/" + h.ref})
			}
		}
		if resp.Headers.Len() == 0 {
			resp.Headers = nil
		}
		out.Set(e.errorResponseName(status), resp)
	}
	return out
}

// rateLimitHeaders are what a refusal carries beside its body.
var rateLimitHeaders = []struct{ name, ref string }{
	{"Retry-After", "RetryAfter"},
	{"RateLimit-Limit", "RateLimitLimit"},
	{"RateLimit-Remaining", "RateLimitRemaining"},
	{"RateLimit-Reset", "RateLimitReset"},
}

// sharedParameters are the request headers every generated handler reads and no
// endpoint declares.
//
// ir.EndpointRequest.Headers exists and no compiled document fills it: these
// are added by the server generator rather than projected onto an endpoint. So
// they are named once here and referenced, instead of restated on several
// hundred operations.
func (e *emitter) sharedParameters() *orderedmap.Map[string, *v3.Parameter] {
	out := orderedmap.New[string, *v3.Parameter]()

	out.Set("ApiRevision", &v3.Parameter{
		Name: genutil.RevisionHeader(e.doc),
		In:   "header",
		Description: "The API revision this client was built against. A server generated from " +
			"an older document than the caller expects answers 426 rather than guessing.",
		Required: boolPtr(false),
		Schema:   base.CreateSchemaProxy(&base.Schema{Type: []string{"string"}}),
	})

	// Only when an operation referred to it. A project whose tables expose
	// nothing but reads has no write to retry, and declaring the header anyway
	// would leave a parameter nothing points at.
	if e.usedIdempotencyKey {
		out.Set("IdempotencyKey", &v3.Parameter{
			Name: "Idempotency-Key",
			In:   "header",
			Description: "A key of the caller's choosing, so a retried write happens once. " +
				"Sending the same key with the same body replays the first answer and sets " +
				"Idempotency-Replayed; with a different body it is refused with 422, and " +
				"while the first is still in flight, with 409.",
			Required: boolPtr(false),
			Schema:   base.CreateSchemaProxy(&base.Schema{Type: []string{"string"}}),
		})
	}

	return out
}

// sharedHeaders are the response headers referenced from more than one place.
func (e *emitter) sharedHeaders() *orderedmap.Map[string, *v3.Header] {
	str := func(desc string) *v3.Header {
		return &v3.Header{
			Description: desc,
			Schema:      base.CreateSchemaProxy(&base.Schema{Type: []string{"string"}}),
		}
	}
	num := func(desc string) *v3.Header {
		return &v3.Header{
			Description: desc,
			Schema:      base.CreateSchemaProxy(&base.Schema{Type: []string{"integer"}}),
		}
	}

	out := orderedmap.New[string, *v3.Header]()
	// Only when a response will reference it. The server sets the header only
	// for a document that has been stamped, so declaring it regardless would
	// leave an unused component in every project that has not generated a
	// revision yet.
	if e.doc.API.Revision != "" {
		out.Set("ApiRevision", str("The revision this server was generated from."))
	}
	// The same rule, for the same reason: no idempotent write, nothing to
	// replay, and no response that points here.
	if e.usedIdempotencyReplayed {
		out.Set("IdempotencyReplayed", &v3.Header{
			Description: "Present and true when this answer is a replay of an earlier request " +
				"carrying the same Idempotency-Key, rather than work done again.",
			Schema: base.CreateSchemaProxy(&base.Schema{Type: []string{"boolean"}}),
		})
	}
	// And again: these four hang off the 429 response and nothing else, so a
	// document no operation can be refused from has no use for them.
	if e.usedStatuses[429] {
		out.Set("RateLimitLimit", num("Requests permitted in the current window."))
		out.Set("RateLimitRemaining", num("Requests left in the current window."))
		out.Set("RateLimitReset", num("Seconds until the current window resets."))
		out.Set("RetryAfter", num("Seconds to wait before retrying."))
	}
	return out
}

// securitySchemes describes the credential, for a project that has one.
//
// One scheme, not two. Both credentials arrive in the same header in the same
// form — Authorization: Bearer <token> — and are told apart by a prefix on the
// token itself, which is why the prefixes exist at all. OpenAPI's
// securitySchemes describes how a credential goes on the wire, and there is one
// way; declaring a second apiKey scheme naming the same header would be true
// and would produce two Authorize boxes writing one header, and two mutually
// exclusive configurations in any generated client.
func (e *emitter) securitySchemes() *orderedmap.Map[string, *v3.SecurityScheme] {
	auth := e.doc.API.Auth
	if auth == nil {
		return nil
	}

	desc := "A credential in `Authorization: Bearer <token>`.\n\n" +
		"Two kinds are accepted and are told apart by a prefix on the token itself. A " +
		"session access token travels on every request and is good for " +
		auth.Session.AccessTTL.String() + ", renewed against the session endpoints before it " +
		"expires. An API key does not expire: an integration key's scopes are its " +
		"permissions outright, and a personal key carries what its owner carries and no more."

	if auth.BasePath != "" {
		desc += "\n\nThe endpoints that issue and renew these are mounted under `" +
			auth.BasePath + "` and are not described in this document."
	}

	out := orderedmap.New[string, *v3.SecurityScheme]()
	out.Set(securitySchemeName, &v3.SecurityScheme{
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "opaque",
		Description:  desc,
	})
	return out
}

// requireCredential is the document-level default: every operation needs one
// unless it says otherwise.
func requireCredential() []*base.SecurityRequirement {
	req := orderedmap.New[string, []string]()
	req.Set(securitySchemeName, []string{})
	return []*base.SecurityRequirement{{Requirements: req}}
}

// optionalCredential is what a public endpoint gets.
//
// Not an empty list. ir.Endpoint.Public means the endpoint does not require the
// claims lookup to succeed — not that a credential is ignored: a caller who
// presents one is still identified by it, and may see more than a stranger
// does. `security: []` would say the opposite. An empty requirement object
// beside the real one is how OpenAPI spells "authentication is optional here".
func optionalCredential() []*base.SecurityRequirement {
	return []*base.SecurityRequirement{
		{Requirements: orderedmap.New[string, []string](), ContainsEmptyRequirement: true},
		requireCredential()[0],
	}
}

// jsonContent is a single application/json body of the given schema.
func jsonContent(ref string) *orderedmap.Map[string, *v3.MediaType] {
	out := orderedmap.New[string, *v3.MediaType]()
	out.Set(ir.MediaJSON, &v3.MediaType{Schema: base.CreateSchemaProxyRef(ref)})
	return out
}

// setExtension adds an x- key, creating the map on first use.
func setExtension(ext **orderedmap.Map[string, *yaml.Node], key string, value *yaml.Node) {
	if *ext == nil {
		*ext = orderedmap.New[string, *yaml.Node]()
	}
	(*ext).Set(key, value)
}

// stringSeq renders a list of strings as a YAML sequence node.
func stringSeq(values []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range values {
		n.Content = append(n.Content, scalarNode("!!str", v))
	}
	return n
}
