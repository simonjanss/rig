package compile

import "github.com/simonjanss/rig/pkg/ir"

// Names rig reserves for the shapes it injects. A resource or field that
// collides with one of these is renamed rather than silently overwriting it.
const (
	EnumErrorCode    = "ErrorCode"
	ObjectError      = "Error"
	ObjectPagination = "Pagination"
	// ObjectRigFile is what an upload answers with and what a file column points
	// at. It is spelled the way projecting [FileTable] would spell it, because
	// with `files.expose` set the projection produces exactly this name and one
	// of the two has to win — an exposed project with two structs for one row
	// would leave the upload method picking between them.
	ObjectRigFile = "RigFile"
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
	{"TooLarge", 413, "The request body is larger than this endpoint accepts."},
	{"UnsupportedMediaType", 415, "The body's content type is not one this endpoint accepts."},
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

// rigFileObject is one uploaded file, as a client sees it.
//
// It is injected whether or not `files.expose` projects the table, because a
// project that never syncs a file row still uploads to one and an upload has to
// answer with something. Where the projection does happen it produces the same
// name and [Expand]'s guard keeps whichever arrived first, so the two spellings
// have to agree — which is why the fields here are exactly the ones rig_file's
// own table configuration exposes, and in the same order.
//
// What is not here is the point of it. The storage key, the checksum, the
// declared content type and the tenant are the server's bookkeeping. The
// storage key is the one that would actually matter: it is what a signed URL is
// built from, and putting it in a shape a client syncs is the same class of
// mistake as syncing a password hash.
func rigFileObject(wire func(string) string) ir.Object {
	return ir.Object{
		Name:        ObjectRigFile,
		Description: "One uploaded file. Metadata only; the bytes are fetched from Url.",
		Origin:      ir.OriginBuiltin,
		Fields: []ir.Field{
			{
				Name: "ID", Wire: wire("ID"),
				Type: ir.TypeUUID, TypeKind: ir.TypeKindPrimitive, GoType: "uuid.UUID",
				Description: "Identifier of the file.",
			},
			{
				Name: "URL", Wire: wire("URL"),
				Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "*string",
				Modifiers: []string{ir.ModifierNullable},
				Description: "Where the file is served from. Stable and unsigned, so it is safe " +
					"to keep and to sync and grants nothing on its own: the endpoint behind " +
					"it still checks the caller. Null until the upload is finished.",
			},
			{
				Name: "FileName", Wire: wire("FileName"),
				Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "string",
				Description: "What the file was called when it arrived. It is for the save " +
					"dialog, and it is not a path: if you write the file somewhere, you " +
					"decide where and you sanitize what.",
			},
			{
				Name: "ContentType", Wire: wire("ContentType"),
				Type: ir.TypeString, TypeKind: ir.TypeKindPrimitive, GoType: "string",
				Description: "What the bytes were sniffed to be, which is what a download " +
					"announces. It is not necessarily what the client claimed on the way up.",
			},
			{
				Name: "SizeBytes", Wire: wire("SizeBytes"),
				Type: ir.TypeInt64, TypeKind: ir.TypeKindPrimitive, GoType: "int64",
				Description: "How large the file is, in bytes.",
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
