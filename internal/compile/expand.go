package compile

import (
	"slices"
	"strconv"

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
		Name:        api.Name,
		Version:     api.Version,
		Description: api.Description,
		BasePath:    api.BasePath,
		Enums:       slices.Clone(api.Enums),
		Objects:     slices.Clone(api.Objects),
		Resources:   make([]ir.Resource, 0, len(api.Resources)),
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

	for _, res := range api.Resources {
		expanded, objs, d := expandResource(res, n, opt)
		diags.Append(d)

		for _, o := range objs {
			if !have(o.Name) {
				out.Objects = append(out.Objects, o)
			}
		}
		out.Resources = append(out.Resources, expanded)
	}

	return out, diags
}

// expandResource returns the resource with its generated endpoints, plus the
// objects those endpoints refer to.
func expandResource(res ir.Resource, n *naming.Namer, opt ExpandOptions) (ir.Resource, []ir.Object, diag.List) {
	var diags diag.List
	wire := func(s string) string { return n.JSON(s) }

	out := res
	out.Operations = slices.Clone(res.Operations)
	out.Fields = slices.Clone(res.Fields)
	out.Endpoints = slices.Clone(res.Endpoints)

	var objects []ir.Object

	readable := readableFields(res)
	objects = append(objects, ir.Object{
		Name:        res.Name,
		Description: res.Description,
		Origin:      ir.OriginProjected,
		Fields:      readable,
	})

	listResponse := res.Name + "ListResponse"
	if res.Supports(ir.OpList) || res.Supports(ir.OpSearch) {
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

	if res.Supports(ir.OpSearch) {
		objects = append(objects, filterObjects(res, readable, wire)...)
	}

	// Each generated endpoint is skipped when one of the same name already
	// exists, so a hand-written endpoint always wins.
	for _, gen := range []struct {
		op    string
		build func() ir.Endpoint
	}{
		{ir.OpCreate, func() ir.Endpoint { return createEndpoint(res) }},
		{ir.OpGet, func() ir.Endpoint { return getEndpoint(res, wire) }},
		{ir.OpList, func() ir.Endpoint { return listEndpoint(res, listResponse, wire) }},
		{ir.OpSearch, func() ir.Endpoint { return searchEndpoint(res, listResponse, opt.SearchMethod, wire) }},
		{ir.OpUpdate, func() ir.Endpoint { return updateEndpoint(res, wire) }},
		{ir.OpDelete, func() ir.Endpoint { return deleteEndpoint(res, wire) }},
	} {
		if !res.Supports(gen.op) {
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

func createEndpoint(res ir.Resource) ir.Endpoint {
	return ir.Endpoint{
		Name:    ir.OpCreate,
		Method:  "POST",
		Path:    "",
		Summary: "Create a " + res.Name + ".",
		Request: ir.EndpointRequest{
			ContentType: "application/json",
			BodyParams:  writableFields(res, ir.FieldOpCreate),
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 201, ContentType: "application/json",
			BodyObject: res.Name, Description: "The created " + res.Name + ".",
		}},
		Errors: []int{400, 401, 403, 409, 422, 429, 500},
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
			StatusCode: 200, ContentType: "application/json",
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
			StatusCode: 200, ContentType: "application/json",
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
			ContentType: "application/json",
			QueryParams: paginationParams(wire),
			BodyParams: []ir.Field{{
				Name: "Filter", Wire: wire("Filter"),
				Type: res.Name + "Filter", TypeKind: ir.TypeKindObject, GoType: res.Name + "Filter",
				Description: "Conditions rows must satisfy.",
			}},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentType: "application/json",
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
			ContentType: "application/json",
			PathParams:  []ir.Field{idParam(res, wire)},
			BodyParams:  writableFields(res, ir.FieldOpUpdate),
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 200, ContentType: "application/json",
			BodyObject: res.Name, Description: "The updated " + res.Name + ".",
		}},
		Errors: []int{400, 401, 403, 404, 409, 422, 429, 500},
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointGenerated, RepoMethod: "Update",
			ServiceMethod: ir.OpUpdate, HandlerName: ir.OpUpdate + res.Name,
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
