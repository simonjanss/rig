package compile

import "github.com/simonjanss/rig/pkg/ir"

// Names rig reserves for the shapes it injects. A resource or field that
// collides with one of these is renamed rather than silently overwriting it.
const (
	EnumErrorCode    = "ErrorCode"
	ObjectError      = "Error"
	ObjectPagination = "Pagination"
)

// ErrorCode pairs an HTTP status with the machine-readable code clients switch
// on. Statuses alone are too coarse — three different failures all return 400 —
// and codes alone leave clients guessing at retry behavior.
type ErrorCode struct {
	Name        string
	Status      int
	Description string
}

// ErrorCodes is every failure rig can produce. The set is closed: a generated
// handler cannot invent a status the client does not already know about.
var ErrorCodes = []ErrorCode{
	{"BadRequest", 400, "The request was malformed or a parameter could not be parsed."},
	{"Unauthorized", 401, "No valid session or API key was presented."},
	{"Forbidden", 403, "The caller is known but not permitted to do this."},
	{"NotFound", 404, "No such resource, or it belongs to another tenant."},
	{"Conflict", 409, "The request conflicts with the current state of the resource."},
	{"UnprocessableEntity", 422, "The request was well-formed but failed validation."},
	{"RateLimited", 429, "Too many requests. Retry after the interval in the Retry-After header."},
	{"UpgradeRequired", 426, "The client was built against an API revision this server no longer serves."},
	{"Internal", 500, "Something went wrong on the server."},
}

// errorCodeEnum builds the ErrorCode enumeration.
func errorCodeEnum() ir.Enum {
	e := ir.Enum{
		Name:        EnumErrorCode,
		Description: "Machine-readable reason a request failed.",
	}
	for _, c := range ErrorCodes {
		e.Values = append(e.Values, ir.EnumValue{
			Name:        c.Name,
			Wire:        c.Name,
			Description: c.Description,
		})
	}
	return e
}

// errorObject is the body of every failure response.
func errorObject(wire func(string) string) ir.Object {
	return ir.Object{
		Name:        ObjectError,
		Description: "Returned whenever a request fails.",
		Origin:      ir.OriginBuiltin,
		Fields: []ir.Field{
			{
				Name: "Code", Wire: wire("Code"),
				Type: EnumErrorCode, TypeKind: ir.TypeKindEnum, GoType: EnumErrorCode,
				Description: "Machine-readable reason the request failed.",
			},
			{
				Name: "Message", Wire: wire("Message"),
				Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "string",
				Description: "Human-readable explanation. Not intended for clients to parse.",
			},
			{
				Name: "RequestID", Wire: wire("RequestID"),
				Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "string",
				Modifiers:   []string{ir.ModifierNullable},
				Description: "Identifier of this request, for correlating with server logs.",
			},
			{
				Name: "Fields", Wire: wire("Fields"),
				Type: ir.TypeJSON, TypeKind: ir.TypeKindPrimitive, GoType: "any",
				Modifiers: []string{ir.ModifierNullable},
				Description: "Present when the failure was validation. It is shaped like " +
					"the request body that failed — one member per field, holding the " +
					"problem with that field — so a client can put each message beside " +
					"the control it belongs to.",
			},
		},
	}
}

// paginationObject accompanies every list and search response.
func paginationObject(wire func(string) string) ir.Object {
	return ir.Object{
		Name:        ObjectPagination,
		Description: "Where the returned page sits within the full result set.",
		Origin:      ir.OriginBuiltin,
		Fields: []ir.Field{
			{
				Name: "Offset", Wire: wire("Offset"),
				Type: ir.TypeInt, TypeKind: ir.TypeKindPrimitive, GoType: "int",
				Description: "Number of rows skipped before this page.",
			},
			{
				Name: "Limit", Wire: wire("Limit"),
				Type: ir.TypeInt, TypeKind: ir.TypeKindPrimitive, GoType: "int",
				Description: "Maximum number of rows in this page.",
			},
			{
				Name: "Total", Wire: wire("Total"),
				Type: ir.TypeInt64, TypeKind: ir.TypeKindPrimitive, GoType: "int64",
				Description: "Total number of rows matching the query, ignoring pagination.",
			},
		},
	}
}

// Pagination defaults. A limit is always applied: an unbounded list is a
// production incident waiting for the table to grow.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)
