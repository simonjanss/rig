package compile

import (
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// ExpandOptions tune the defaulting pass.
type ExpandOptions struct {
	Namer *naming.Namer
	// SearchMethod selects how Search is exposed.
	SearchMethod string
	// BasePath prefixes every route.
	BasePath string
}

// Expand fills in everything that follows from what the schema and the
// configuration already said: the built-in shapes, one object per resource, the
// filter shapes Search needs, and the CRUD endpoints themselves.
//
// Two properties matter and are tested directly. It is idempotent, so running
// it twice produces the same API as running it once. And it does not modify its
// input: every slice it touches is rebuilt rather than appended to, because
// appending into a slice the caller still holds is a bug that only shows up
// when capacity happens to allow it.
//
// Everything is guarded by name. A hand-written endpoint called Get suppresses
// the generated one, and a hand-declared object called Error replaces the
// built-in — which is what makes the configuration an escape hatch rather than
// a suggestion.
func Expand(api ir.API, opt ExpandOptions) (ir.API, diag.List) {
	var diags diag.List

	n := namerOrDefault(opt.Namer)
	wire := func(s string) string { return n.JSON(s) }

	out := ir.API{
		Name:           api.Name,
		Version:        api.Version,
		Description:    api.Description,
		BasePath:       api.BasePath,
		RevisionHeader: api.RevisionHeader,
		Enums:          slices.Clone(api.Enums),
		Objects:        slices.Clone(api.Objects),
		Resources:      make([]ir.Resource, 0, len(api.Resources)),
		// Expansion adds endpoints to resources; the authentication foundation
		// brings its own and is carried through as it arrived.
		Auth:               api.Auth,
		Files:              api.Files,
		Notifications:      api.Notifications,
		Presence:           api.Presence,
		Throttle:           api.Throttle,
		Cache:              api.Cache,
		Tracing:            api.Tracing,
		Monitoring:         api.Monitoring,
		Servers:            api.Servers,
		OpenAPI:            api.OpenAPI,
		EmbeddedFoundation: api.EmbeddedFoundation,
	}

	have := func(name string) bool {
		for _, o := range out.Objects {
			if o.Name == name {
				return true
			}
		}
		return false
	}
	haveEnum := func(name string) bool {
		for _, e := range out.Enums {
			if e.Name == name {
				return true
			}
		}
		return false
	}

	if !haveEnum(EnumErrorCode) {
		out.Enums = append(out.Enums, errorCodeEnum())
	}
	if !have(ObjectError) {
		out.Objects = append(out.Objects, errorObject(wire))
	}
	if !have(ObjectPagination) {
		out.Objects = append(out.Objects, paginationObject(wire))
	}

	// The authentication trail's shape, for a project that has authentication at
	// all. Here rather than after the resource loop — where the file shape is —
	// because the projection has to lose this one: an exposed rig_auth_log
	// projects to a resource named AuthLog and a table of a project's own could
	// project to AuthLogEntry, and neither is the row this describes. The file
	// shape is the opposite case, where both spellings are the same row.
	//
	// Nothing in the document refers to it, because no /auth/* route does: those
	// are mounted by a hand-written module and have never reached the IR. It is
	// here so that a client and an OpenAPI document can describe the trail
	// endpoint the day they can describe any of them.
	if out.Auth != nil && !have(ObjectAuthLogEntry) {
		out.Objects = append(out.Objects, authLogEntryObject())
	}

	// Which resources a relation may point at. A filter is the wire shape, so a
	// relation to a table the API does not expose is not one a client gets to
	// ask about.
	exposed := make(map[string]bool, len(api.Resources))
	for _, res := range api.Resources {
		if !res.Unexposed {
			exposed[res.Name] = true
		}
	}

	for _, res := range api.Resources {
		expanded, objs, d := expandResource(res, n, opt, exposed)
		diags.Append(d)

		for _, o := range objs {
			if !have(o.Name) {
				out.Objects = append(out.Objects, o)
			}
		}
		out.Resources = append(out.Resources, expanded)
	}

	// The file shape, last, because a projection of the file table beats it.
	//
	// It is injected at all so that `files.expose: false` still has something for
	// an upload to answer with — without it, a project that never syncs a file
	// row would have endpoints returning a type the document does not contain.
	// It is injected last so that a project which does expose the table gets the
	// projected object, which carries the column bindings the persistence and
	// sync generators read and the builtin has no way to know. One row, one
	// struct, either way.
	//
	// Only where something has a file column. A shape nothing references is a
	// type every client carries and nobody can obtain, and `files.enabled` on its
	// own does not put a file on a row.
	if slices.ContainsFunc(out.Resources, func(r ir.Resource) bool { return len(r.Files) > 0 }) &&
		!have(ObjectRigFile) {
		out.Objects = append(out.Objects, rigFileObject(wire))
	}

	return out, diags
}

// expandResource returns the resource with its generated endpoints, plus the
// objects those endpoints refer to.
func expandResource(res ir.Resource, n *naming.Namer, opt ExpandOptions, exposed map[string]bool) (ir.Resource, []ir.Object, diag.List) {
	var diags diag.List
	wire := func(s string) string { return n.JSON(s) }

	out := res
	out.Operations = slices.Clone(res.Operations)
	out.Fields = slices.Clone(res.Fields)
	out.Endpoints = slices.Clone(res.Endpoints)

	var objects []ir.Object

	readable := readableFields(res)

	// The filter is not only Search's request body: it is how anything selects
	// rows, so the model declares it and the repository takes it whether or not
	// the API exposes a search. One shape, so what a client may ask for and what
	// a repository can answer cannot come apart.
	//
	// An unexposed table gets one too — it still has a repository, and that
	// repository still lists. Nothing routes to it, so nothing in the document
	// references it, which is what keeps rig_account_token out of the OpenAPI.
	if res.Storage != nil {
		objects = append(objects, filterObjects(res, readable, wire, exposed)...)
	}

	// An unexposed table has a model and a repository and no API at all, so
	// there is no wire shape to name and nothing to route. Generating them
	// anyway would put rig_account_token in the OpenAPI document.
	if res.Unexposed {
		return out, objects, diags
	}
	objects = append(objects, ir.Object{
		Name:        res.Name,
		Description: res.Description,
		Origin:      ir.OriginProjected,
		Fields:      readable,
	})

	// The history is answered with the same envelope a list is, rather than a
	// second shape for the same thing, so a resource with a history needs it
	// even when nothing else lists.
	listResponse := res.Name + "ListResponse"
	if res.Supports(ir.OpList) || res.Supports(ir.OpSearch) || supportsVersions(res) {
		objects = append(objects, ir.Object{
			Name:        listResponse,
			Description: "A page of " + res.Plural + ".",
			Origin:      ir.OriginProjected,
			Fields: []ir.Field{
				{
					Name: "Data", Wire: wire("Data"),
					Type: res.Name, TypeKind: ir.TypeKindObject, GoType: "[]" + res.Name,
					Modifiers:   []string{ir.ModifierArray},
					Description: "The rows in this page.",
				},
				{
					Name: "Pagination", Wire: wire("Pagination"),
					Type: ObjectPagination, TypeKind: ir.TypeKindObject, GoType: ObjectPagination,
					Description: "Where this page sits in the full result set.",
				},
			},
		})
	}

	// Each generated endpoint is skipped when one of the same name already
	// exists, so a hand-written endpoint always wins.
	for _, gen := range []struct {
		op    string
		when  bool
		build func() ir.Endpoint
	}{
		{ir.OpCreate, res.Supports(ir.OpCreate), func() ir.Endpoint { return createEndpoint(res) }},
		{ir.OpGet, res.Supports(ir.OpGet), func() ir.Endpoint { return getEndpoint(res, wire) }},
		{ir.OpList, res.Supports(ir.OpList), func() ir.Endpoint { return listEndpoint(res, listResponse, wire) }},
		{ir.OpSearch, res.Supports(ir.OpSearch), func() ir.Endpoint {
			return searchEndpoint(res, listResponse, opt.SearchMethod, wire)
		}},
		{ir.OpUpdate, res.Supports(ir.OpUpdate), func() ir.Endpoint { return updateEndpoint(res, wire) }},
		{ir.OpDelete, res.Supports(ir.OpDelete), func() ir.Endpoint { return deleteEndpoint(res, wire) }},
		{ir.OpListDeleted, supportsListDeleted(res), func() ir.Endpoint {
			return listDeletedEndpoint(res, listResponse, wire)
		}},
		{ir.OpRestore, supportsRestore(res), func() ir.Endpoint { return restoreEndpoint(res, wire) }},
		{ir.OpVersions, supportsVersions(res), func() ir.Endpoint {
			return versionsEndpoint(res, listResponse, wire)
		}},
		{ir.OpRevert, supportsRevert(res), func() ir.Endpoint { return revertEndpoint(res, wire) }},
	} {
		if !gen.when {
			continue
		}
		if existing := out.Endpoint(gen.op); existing != nil {
			diags.Add(diag.CodeEndpointShadowed, diag.At(res.Name+".endpoints."+gen.op),
				"%s.%s is hand-written, so the generated %s endpoint is not emitted",
				res.Name, gen.op, gen.op)
			continue
		}
		out.Endpoints = append(out.Endpoints, gen.build())
	}

	// The file endpoints, three per file column, beside the CRUD rather than
	// under a resource of their own.
	//
	// The nesting is the design. It forces a handler to resolve the owning row
	// through the generated repository before it touches a byte, and every
	// generated read is tenant-scoped in SQL and narrowed further by
	// `access: { scope: own }` — so you cannot upload to a row you cannot see,
	// and an owner-scoped table's files are exactly as invisible as its rows.
	// None of that is new machinery. A flat /files/{id} could not reach any of
	// it without a second authorization model beside the first.
	for _, fc := range out.Files {
		builders := []func() ir.Endpoint{
			func() ir.Endpoint { return uploadFileEndpoint(res, fc, wire) },
			func() ir.Endpoint { return downloadFileEndpoint(res, fc, wire) },
		}
		// A not-null file column has no delete. Detaching means clearing the
		// column, and a column the schema says cannot be null has nowhere to be
		// cleared to — the row goes instead, which is the ordinary delete. This
		// is the schema deciding, the way `deleted_at` decides whether there is
		// a restore.
		if !fc.Required {
			builders = append(builders, func() ir.Endpoint { return deleteFileEndpoint(res, fc, wire) })
		}

		for _, build := range builders {
			ep := build()
			if existing := out.Endpoint(ep.Name); existing != nil {
				diags.Add(diag.CodeEndpointShadowed, diag.At(res.Name+".endpoints."+ep.Name),
					"%s.%s is hand-written, so the generated %s endpoint is not emitted",
					res.Name, ep.Name, ep.Name)
				continue
			}
			out.Endpoints = append(out.Endpoints, ep)
		}
	}

	// The table's public list is resolved here, once, over everything the
	// resource ended up with. A custom endpoint can also say so in its own
	// block — that is already on the endpoint by now — and this is the other
	// end of the same setting rather than a second one.
	for _, name := range res.Public {
		ep := out.Endpoint(name)
		if ep == nil {
			// A name matching nothing reads as "this is open" while meaning
			// nothing at all, which is the worst way for a typo in a security
			// setting to fail.
			diags.Add(diag.CodeInvalidEndpoint, diag.At(res.Name+".public"),
				"%s has no operation or endpoint named %q to make public", res.Name, name)
			continue
		}
		ep.Public = true
	}

	// After the public list, because a public read gets no scope parameter: there
	// is no caller to own anything, so "own" has no meaning and offering a
	// parameter nothing can authorize would just be a 403 generator.
	addScopeParam(&out)

	return out, objects, diags
}

// readableFields are the fields a client sees when it reads the resource.
func readableFields(res ir.Resource) []ir.Field {
	var out []ir.Field
	for _, f := range res.Fields {
		if f.In(ir.FieldOpRead) {
			out = append(out, f.Field)
		}
	}
	return out
}

// writableFields are the fields accepted for one write operation. Immutable
// fields appear on create and are absent from update entirely, which is what
// makes "you cannot change this" a compile error rather than a runtime one.
func writableFields(res ir.Resource, op string) []ir.Field {
	var out []ir.Field
	for _, f := range res.Fields {
		if !f.In(op) || f.ReadOnly {
			continue
		}
		if op == ir.FieldOpUpdate && f.Immutable {
			continue
		}
		out = append(out, f.Field)
	}
	return out
}

// addScopeParam gives every generated read of an owner-scoped resource the
// parameter that widens it.
//
// Only the generated reads. A custom endpoint decides for itself what it answers
// with — that is why somebody wrote it — and quietly adding a parameter its
// handler ignores would describe behaviour the code does not have.
//
// Writes get nothing. An owner-scoped table refuses to change a row the caller
// did not create, full stop, and there is no parameter to say otherwise: every
// generated write reads the row first, so the narrow read *is* the write rule. A
// table that needs an integration to change rows across the tenant should not
// declare itself owner-scoped.
func addScopeParam(res *ir.Resource) {
	if !res.Storage.IsOwnerScoped() {
		return
	}
	wide := wideReadPermission(res.Storage.Table)

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		if ep.Impl.Kind != ir.EndpointGenerated || ep.Public {
			continue
		}
		switch ep.Name {
		case ir.OpGet, ir.OpList, ir.OpSearch, ir.OpListDeleted, ir.OpVersions:
		default:
			continue
		}

		ep.WidePermission = wide
		ep.Request.QueryParams = append(ep.Request.QueryParams, scopeParam(res, wide))
	}
}

// scopeParam is the reserved parameter, spelled the same on every endpoint of
// every resource.
//
// Its wire name is not run through the project's naming rules the way a field's
// is. It is one lowercase word in every casing anybody configures, and a
// parameter the framework owns should not be something a client has to look up
// per project.
func scopeParam(res *ir.Resource, wide string) ir.Field {
	return ir.Field{
		Name: "Scope", Wire: ir.ScopeParam,
		Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "tenancy.Scope",
		Default: AccessScopeOwn,
		Description: "How wide to read: " + AccessScopeOwn + " for the " +
			strings.ToLower(res.Plural) + " the caller created, which is the default, or " +
			AccessScopeAll + " for every one in the tenant. Asking for " + AccessScopeAll +
			" requires the " + wide + " permission and is refused without it, rather than " +
			"quietly answering with fewer rows.",
	}
}

func idParam(res ir.Resource, wire func(string) string) ir.Field {
	return ir.Field{
		Name: "ID", Wire: wire("ID"),
		Type: ir.TypeUUID, TypeKind: ir.TypeKindPrimitive, GoType: "uuid.UUID",
		Description: "Identifier of the " + res.Name + ".",
	}
}

func paginationParams(wire func(string) string) []ir.Field {
	return []ir.Field{
		{
			Name: "Limit", Wire: wire("Limit"),
			Type: ir.TypeInt, TypeKind: ir.TypeKindPrimitive, GoType: "int",
			Default:     strconv.Itoa(DefaultLimit),
			Description: "Maximum rows to return.",
		},
		{
			Name: "Offset", Wire: wire("Offset"),
			Type: ir.TypeInt, TypeKind: ir.TypeKindPrimitive, GoType: "int",
			Default:     "0",
			Description: "Rows to skip before the first returned row.",
		},
	}
}

// createEndpoint makes a row.
//
// On a table with a file column it accepts a form as well as a JSON body, and
// that is not a convenience. Without it, adding a row with a file is two
// requests: the file column could never be `not null`, because the row has to
// exist before an upload has anywhere to go, and a client that makes the first
// request and not the second leaves a row rig has no business sweeping — it is
// your table, and rig cannot know the row is meaningless.
//
// The form is the same body in a part named "json", plus one part per file
// column. JSON stays first in the list, so a generator that wants "the" content
// type is unchanged, and a request that arrives without a multipart content
// type behaves exactly as it did before this existed.
//
// Create only. Replacing a file on a row that exists is what the upload
// endpoint is for, and an update that could also carry bytes would be two ways
// to do one thing with different transactional shapes.
func createEndpoint(res ir.Resource) ir.Endpoint {
	content := []string{MediaJSON}
	errors := []int{400, 401, 403, 409, 422, 429, 500}

	var parts []ir.FilePart
	for _, fc := range res.Files {
		parts = append(parts, filePartOf(fc))
	}
	if len(parts) > 0 {
		content = append(content, MediaMultipart)
		// The two an upload can answer and a JSON body cannot. They are listed
		// on the endpoint rather than only on the upload route because this is
		// the same endpoint, and a client that sends a form here can be told the
		// file is too large or of a type the project refuses.
		errors = append(errors, 413, 415)
		slices.Sort(errors)
	}

	return ir.Endpoint{
		Name:    ir.OpCreate,
		Method:  "POST",
		Path:    "",
		Summary: "Create a " + res.Name + ".",
		Request: ir.EndpointRequest{
			ContentTypes: content,
			FileParts:    parts,
			BodyParams:   writableFields(res, ir.FieldOpCreate),
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 201, ContentTypes: []string{MediaJSON},
			BodyObject: res.Name, Description: "The created " + res.Name + ".",
		}},
		Errors: errors,
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Create",
			ServiceMethod: ir.OpCreate, HandlerName: ir.OpCreate + res.Name,
		},
	}
}

func getEndpoint(res ir.Resource, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpGet,
		Method:  "GET",
		Path:    "/{id}",
		Summary: "Fetch one " + res.Name + " by identifier.",
		Request: ir.EndpointRequest{PathParams: []ir.Field{idParam(res, wire)}},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: res.Name, Description: "The requested " + res.Name + ".",
		}},
		Errors: []int{400, 401, 403, 404, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Get",
			ServiceMethod: ir.OpGet, HandlerName: ir.OpGet + res.Name,
		},
	}
}

func listEndpoint(res ir.Resource, response string, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpList,
		Method:  "GET",
		Path:    "",
		Summary: "List " + res.Plural + ".",
		Request: ir.EndpointRequest{QueryParams: paginationParams(wire)},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: response, Description: "A page of " + res.Plural + ".",
		}},
		Errors: []int{400, 401, 403, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "List",
			ServiceMethod: ir.OpList, HandlerName: ir.OpList + res.Plural,
		},
	}
}

// searchEndpoint exposes filtered reads.
//
// QUERY is the method for a read that carries a body: safe, idempotent, and
// cacheable, none of which POST can claim. The POST alias exists because some
// intermediaries still reject methods they do not recognize, and a client
// behind one of them should degrade rather than fail.
func searchEndpoint(res ir.Resource, response, method string, wire func(string) string) ir.Endpoint {
	e := ir.Endpoint{
		Name:    ir.OpSearch,
		Method:  "QUERY",
		Path:    "",
		Summary: "Search " + res.Plural + " with filters.",
		Request: ir.EndpointRequest{
			ContentTypes: []string{MediaJSON},
			QueryParams:  paginationParams(wire),
			BodyParams: []ir.Field{{
				Name: "Filter", Wire: wire("Filter"),
				Type: res.Name + "Filter", TypeKind: ir.TypeKindObject, GoType: res.Name + "Filter",
				Description: "Conditions rows must satisfy.",
			}},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: response, Description: "A page of matching " + res.Plural + ".",
		}},
		Errors: []int{400, 401, 403, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "List",
			ServiceMethod: ir.OpSearch, HandlerName: ir.OpSearch + res.Plural,
		},
	}

	switch method {
	case "post":
		e.Method = "POST"
		e.Path = "/_search"
	case "query":
		// QUERY only.
	default: // both
		e.AliasPatterns = []string{"POST /_search"}
	}
	return e
}

func updateEndpoint(res ir.Resource, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpUpdate,
		Method:  "PATCH",
		Path:    "/{id}",
		Summary: "Update a " + res.Name + ".",
		Description: "Only the fields present in the body are changed. A field set to null is " +
			"cleared; a field left out is left alone.",
		Request: ir.EndpointRequest{
			ContentTypes: []string{MediaJSON},
			PathParams:   []ir.Field{idParam(res, wire)},
			BodyParams:   writableFields(res, ir.FieldOpUpdate),
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: res.Name, Description: "The updated " + res.Name + ".",
		}},
		Errors: []int{400, 401, 403, 404, 409, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Update",
			ServiceMethod: ir.OpUpdate, HandlerName: ir.OpUpdate + res.Name,
		},
	}
}

// supportsListDeleted reports whether the trash is exposed.
//
// It follows List rather than Delete: a deletion can be made by service code
// that no route offers, so what decides here is whether listing rows is
// something this resource does at all.
func supportsListDeleted(res ir.Resource) bool {
	return res.Storage.IsSoftDeletable() && res.Supports(ir.OpList)
}

// listDeletedEndpoint reads the rows that were retired.
func listDeletedEndpoint(res ir.Resource, response string, wire func(string) string) ir.Endpoint {
	window := res.Storage.SoftDelete.RestoreWindowDays

	return ir.Endpoint{
		Name:    ir.OpListDeleted,
		Method:  "GET",
		Path:    "/_deleted",
		Summary: "List retired " + res.Plural + ".",
		Description: "A deletion stamps the row rather than removing it, so this is what " +
			"was deleted and can still be brought back. Only rows inside the " +
			strconv.Itoa(window) + "-day restore window appear: past it a row is gone as far " +
			"as anyone is concerned, so it is not in the trash either.",
		Request: ir.EndpointRequest{QueryParams: paginationParams(wire)},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: response, Description: "A page of retired " + res.Plural + ".",
		}},
		Errors: []int{400, 401, 403, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "ListDeleted",
			ServiceMethod: ir.OpListDeleted, HandlerName: ir.OpListDeleted + res.Plural,
		},
	}
}

// supportsRestore reports whether a retired row can be brought back.
//
// It follows Update: restoring is a write, and a resource that does not expose
// writes must not gain one through the back door. A resource that exposes
// Delete without Update is one where retiring a row is meant to be final.
func supportsRestore(res ir.Resource) bool {
	return res.Storage.IsSoftDeletable() && res.Supports(ir.OpUpdate)
}

// restoreEndpoint brings a retired row back.
func restoreEndpoint(res ir.Resource, wire func(string) string) ir.Endpoint {
	window := res.Storage.SoftDelete.RestoreWindowDays

	return ir.Endpoint{
		Name:    ir.OpRestore,
		Method:  "POST",
		Path:    "/{id}/_restore",
		Summary: "Bring a retired " + res.Name + " back.",
		Description: "The deletion stamp is cleared and the row appears in reads again. " +
			"It works for " + strconv.Itoa(window) + " days after the deletion; past that " +
			"the row is no longer eligible and this answers 409.\n\n" +
			"Restoring a row that was never deleted is not an error. It is already in " +
			"the state the caller asked for, and answering otherwise would make a " +
			"retry of a request whose response went missing look like a failure.\n\n" +
			"The request carries no fields. What to do when the world has moved on " +
			"while the row was retired — a unique value taken by something created " +
			"since — is the application's decision, and it is made in the restore " +
			"hook: refuse, or change the row so that it fits.\n\n" +
			"Every rule then runs against whatever the hook settled on, not only the " +
			"ones for fields it touched. The row was not live, so nothing about it has " +
			"been checked against the world it is returning to.",
		Request: ir.EndpointRequest{PathParams: []ir.Field{idParam(res, wire)}},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: res.Name, Description: "The restored " + res.Name + ".",
		}},
		Errors: []int{400, 401, 403, 404, 409, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Restore",
			ServiceMethod: ir.OpRestore, HandlerName: ir.OpRestore + res.Name,
		},
	}
}

// supportsVersions reports whether the history is exposed.
//
// A table that keeps its previous versions can be asked for them — but only
// where the row itself can be read. Offering the history of something a client
// cannot fetch would be an odd door to leave open.
func supportsVersions(res ir.Resource) bool {
	return res.Storage.IsSnapshotable() && res.Supports(ir.OpGet)
}

// supportsRevert reports whether a previous version can be put back.
//
// Reverting is an update — it goes through the same path, snapshots what it
// replaces, and runs the same rules — so a resource that does not expose
// updates does not expose this either. Otherwise it would be a second way to
// write a row, with a different set of permissions and the same effect.
func supportsRevert(res ir.Resource) bool {
	return supportsVersions(res) && res.Supports(ir.OpUpdate)
}

// versionsEndpoint reads a row's history.
func versionsEndpoint(res ir.Resource, response string, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpVersions,
		Method:  "GET",
		Path:    "/{id}/_versions",
		Summary: "List a " + res.Name + "'s previous versions.",
		Description: "Every update copies the row as it was before writing the change, so " +
			"this is the row's history: newest first, and never the live row itself.\n\n" +
			"These do not appear in ordinary reads. A version answers Get by its own " +
			"identifier, but it is filtered out of List and Search, so history does not " +
			"turn up in a listing.",
		Request: ir.EndpointRequest{PathParams: []ir.Field{idParam(res, wire)}},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: response, Description: "The versions of the " + res.Name + ", newest first.",
		}},
		Errors: []int{400, 401, 403, 404, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "ListSnapshots",
			ServiceMethod: ir.OpVersions, HandlerName: ir.OpVersions + "Of" + res.Name,
		},
	}
}

// revertEndpoint puts a previous version back.
func revertEndpoint(res ir.Resource, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpRevert,
		Method:  "POST",
		Path:    "/{id}/_revert",
		Summary: "Put a " + res.Name + " back to one of its previous versions.",
		Description: "The version is replayed through the ordinary update path rather than " +
			"written over the row, so the state being replaced is itself snapshotted " +
			"first and every rule an update runs still runs. Reverting is undoable, and " +
			"a value that would be refused now is still refused.",
		Request: ir.EndpointRequest{
			ContentTypes: []string{MediaJSON},
			PathParams:   []ir.Field{idParam(res, wire)},
			BodyParams: []ir.Field{{
				Name: "VersionID", Wire: wire("VersionID"),
				Type: ir.TypeUUID, TypeKind: ir.TypeKindPrimitive, GoType: "uuid.UUID",
				Description: "The version to put back, from the " + res.Name + "'s history.",
			}},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentTypes: []string{MediaJSON},
			BodyObject: res.Name, Description: "The " + res.Name + ", as of the version replayed.",
		}},
		// 404 covers a version that is not one of this row's, deliberately: a
		// version identifier is a row identifier, and saying "that exists but
		// is not yours" is telling the caller about somebody else's row.
		Errors: []int{400, 401, 403, 404, 409, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Revert",
			ServiceMethod: ir.OpRevert, HandlerName: ir.OpRevert + res.Name,
		},
	}
}

func deleteEndpoint(res ir.Resource, wire func(string) string) ir.Endpoint {
	e := ir.Endpoint{
		Name:    ir.OpDelete,
		Method:  "DELETE",
		Path:    "/{id}",
		Summary: "Delete a " + res.Name + ".",
		Request: ir.EndpointRequest{PathParams: []ir.Field{idParam(res, wire)}},
		Responses: []ir.EndpointResponse{{
			StatusCode: 204, Description: "The " + res.Name + " was deleted.",
		}},
		Errors: []int{400, 401, 403, 404, 409, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Delete",
			ServiceMethod: ir.OpDelete, HandlerName: ir.OpDelete + res.Name,
		},
	}

	if res.Storage.IsSoftDeletable() {
		e.Description = "The row is retired by stamping a deletion time; it stops appearing in " +
			"reads but can be restored within the retention window."
	}
	return e
}

// uploadFileEndpoint attaches bytes to an existing row.
func uploadFileEndpoint(res ir.Resource, fc ir.FileColumn, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    "Upload" + fc.GoName(),
		Method:  "POST",
		Path:    "/{id}/" + fc.Segment,
		Summary: "Attach a file to a " + res.Name + " as its " + humanRole(fc) + ".",
		Description: "The form carries one part, named " + fc.Part + ". Nothing else in it is " +
			"accepted.\n\n" +
			"The type is sniffed from the bytes rather than taken from what the " +
			"request claimed, and the sniffed type is what is stored and what a " +
			"download announces. A file replacing an existing one does not delete " +
			"it: a row that keeps its previous versions still points at the old " +
			"file from every one of them.",
		File: &fc,
		Request: ir.EndpointRequest{
			ContentTypes: []string{MediaMultipart},
			PathParams:   []ir.Field{idParam(res, wire)},
			FileParts:    []ir.FilePart{filePartOf(fc)},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 201, ContentTypes: []string{MediaJSON},
			BodyObject: ObjectRigFile, Description: "The stored file.",
		}},
		Errors: []int{400, 401, 403, 404, 409, 413, 415, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Get",
			ServiceMethod: "Upload" + fc.GoName(),
			HandlerName:   "Upload" + res.Name + fc.GoName(),
		},
	}
}

// downloadFileEndpoint serves the bytes back.
//
// The filename is the last segment for the browser's save dialog and for cache
// busting, and it is not the lookup: the file identifier is. It is compared
// against the stored name and a mismatch is a 404, and it never goes near the
// storage key.
//
// Bytes flow through the API rather than through a signed storage URL, because
// the endpoint is where the permission check lives. That is the cost of the URL
// on the row being stable and unsigned, and it is the reason syncing it is safe.
func downloadFileEndpoint(res ir.Resource, fc ir.FileColumn, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    "Download" + fc.GoName(),
		Method:  "GET",
		Path:    "/{id}/" + fc.Segment + "/{fileId}/{filename}",
		Summary: "Fetch a " + res.Name + "'s " + humanRole(fc) + ".",
		Description: "Answers range and conditional requests, so a resumed download does not " +
			"start over and a media element can seek.",
		File: &fc,
		Request: ir.EndpointRequest{
			PathParams: []ir.Field{
				idParam(res, wire),
				{
					Name: "FileID", Wire: wire("FileID"),
					Type: ir.TypeUUID, TypeKind: ir.TypeKindPrimitive, GoType: "uuid.UUID",
					Description: "Identifier of the file, from the row's " + fc.Role + " field.",
				},
				{
					Name: "Filename", Wire: wire("Filename"),
					Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "string",
					Description: "The file's own name. It has to match the stored one, and it " +
						"is what the browser offers to save the download as.",
				},
			},
		},
		Responses: []ir.EndpointResponse{
			{
				StatusCode: 200, ContentTypes: []string{MediaOctet},
				Description: "The file's bytes, announced as whatever they were sniffed to be.",
			},
			{
				StatusCode: 206, ContentTypes: []string{MediaOctet},
				Description: "The requested range of the file.",
			},
			{
				StatusCode:  304,
				Description: "The caller already has this version of the file.",
			},
		},
		Errors: []int{400, 401, 403, 404, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Get",
			ServiceMethod: "Download" + fc.GoName(),
			HandlerName:   "Download" + res.Name + fc.GoName(),
		},
	}
}

// deleteFileEndpoint detaches the file and retires it.
func deleteFileEndpoint(res ir.Resource, fc ir.FileColumn, wire func(string) string) ir.Endpoint {
	return ir.Endpoint{
		Name:    "Delete" + fc.GoName(),
		Method:  "DELETE",
		Path:    "/{id}/" + fc.Segment,
		Summary: "Remove a " + res.Name + "'s " + humanRole(fc) + ".",
		Description: "The column is cleared and the file is retired. Its bytes outlive the " +
			"delete for as long as the file restore window, because a restore inside " +
			"that window has to hand back a row pointing at something.",
		File: &fc,
		Request: ir.EndpointRequest{
			PathParams: []ir.Field{idParam(res, wire)},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 204, Description: "The file was removed.",
		}},
		Errors: []int{400, 401, 403, 404, 409, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Get",
			ServiceMethod: "Delete" + fc.GoName(),
			HandlerName:   "Delete" + res.Name + fc.GoName(),
		},
	}
}

// filePartOf is the file column as a part of a form.
func filePartOf(fc ir.FileColumn) ir.FilePart {
	return ir.FilePart{
		Name:     fc.Part,
		Field:    fc.Field,
		Role:     fc.Role,
		Required: fc.Required,
	}
}

// humanRole is the role as a sentence reads it: "profile image" rather than
// "profileImage".
func humanRole(fc ir.FileColumn) string {
	return strings.ReplaceAll(strings.TrimSuffix(fc.Segment, "-file"), "-", " ")
}
