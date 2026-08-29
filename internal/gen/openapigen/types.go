package openapigen

import (
	"strconv"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	"go.yaml.in/yaml/v4"

	"github.com/simonjanss/rig/pkg/ir"
)

// schemaRef is where a named shape lives in the document.
func schemaRef(name string) string { return "#/components/schemas/" + name }

// primitiveSchema is the JSON Schema for one of the IR's primitive types.
//
// The mapping answers to what the server actually puts on the wire, which is
// not always what the type's name suggests. Two of them are worth the words:
//
// Date and Time are date-time, not date and time. Both columns scan into a
// time.Time (internal/pgtypes), so encoding/json writes a full RFC3339 stamp
// for all three — which is also why the Go client formats all three with
// time.RFC3339Nano. The IR keeps the distinction because Postgres does; the
// wire throws it away, and a specification describes the wire. Do not
// "correct" this to format: date, or the document will fail its own examples.
//
// Decimal is a number, and rig's own comment beside the pgtypes table says it
// should have been a string — see decimalNote.
func primitiveSchema(t string) *base.Schema {
	switch t {
	case ir.TypeBool:
		return &base.Schema{Type: []string{"boolean"}}
	case ir.TypeBytes:
		return &base.Schema{Type: []string{"string"}, ContentEncoding: "base64"}
	case ir.TypeDate, ir.TypeTime, ir.TypeTimestamp:
		return &base.Schema{Type: []string{"string"}, Format: "date-time"}
	case ir.TypeDecimal:
		return &base.Schema{Type: []string{"number"}, Format: "decimal"}
	case ir.TypeFloat64:
		return &base.Schema{Type: []string{"number"}, Format: "double"}
	case ir.TypeInt:
		return &base.Schema{Type: []string{"integer"}, Format: "int32"}
	case ir.TypeInt64:
		return &base.Schema{Type: []string{"integer"}, Format: "int64"}
	case ir.TypeJSON:
		// No type keyword at all. A jsonb column holds anything, and any
		// narrowing here would be a claim about a column that made no promise.
		return &base.Schema{}
	case ir.TypeString:
		return &base.Schema{Type: []string{"string"}}
	case ir.TypeUUID:
		return &base.Schema{Type: []string{"string"}, Format: "uuid"}
	}
	// An unrecognised primitive is a compiler change this generator has not
	// caught up with. An empty schema is the honest answer — it says nothing
	// rather than something wrong — and the round-trip test names it.
	return &base.Schema{}
}

// decimalNote is appended to every Decimal field's description.
//
// pgtype.Numeric marshals as a bare JSON number, so that is what the schema
// says. Two things follow that a reader has to be told, because neither is
// visible in `type: number`: a NaN arrives as the string "NaN" and will not
// validate against this schema, and a value with more significant digits than
// an IEEE double holds loses precision in any client that parses JSON numbers
// as doubles. Both are properties of the server as it stands rather than of
// this document.
const decimalNote = "Sent as a JSON number. A not-a-number value arrives as the string \"NaN\" " +
	"instead, and a value with more than about fifteen significant digits is subject to " +
	"the precision of the reader's own number type."

// registeredFormats are the ir.Field.Format values with a format keyword the
// JSON Schema registry actually defines.
//
// The other six — PhoneNumber, Color, CountryCode, LanguageCode, TimeZone and
// RichText — get a sentence in the description and no keyword. Nothing in rig
// validates any of the eight: a column's format is a hint the compiler carries
// and no handler checks. Emitting a pattern for them would be rig promising a
// constraint it does not enforce, which turns data the server accepts into data
// a client refuses to send. And rig has not decided whether RichText is HTML or
// Markdown, or whether LanguageCode is BCP 47 or ISO 639-1, so even an
// annotation would be picking an answer nothing else in the codebase has.
var registeredFormats = map[string]string{
	"EmailAddress": "email",
	// uri, not url. url is not a registered format, and linters say so.
	"URL": "uri",
}

// formatNotes describe the formats OpenAPI has no keyword for.
var formatNotes = map[string]string{
	"PhoneNumber":  "A telephone number.",
	"Color":        "A colour.",
	"CountryCode":  "A country code.",
	"LanguageCode": "A language code.",
	"TimeZone":     "A time zone name.",
	"RichText":     "Formatted text rather than plain.",
}

// fieldSchema renders one field, modifiers and all.
//
// Nullability is a type union rather than the nullable keyword: 3.1 is JSON
// Schema 2020-12, where null is a type. On a $ref it has to be a oneOf instead,
// because {$ref: X, type: "null"} asks for both at once and nothing satisfies
// that.
func (e *emitter) fieldSchema(f ir.Field) *base.SchemaProxy {
	inner, isRef := e.baseSchema(f)

	switch {
	case f.IsArray():
		items := base.CreateSchemaProxy(inner)
		if isRef {
			items = base.CreateSchemaProxyRef(schemaRef(f.Type))
		}
		out := &base.Schema{
			Type:  []string{"array"},
			Items: &base.DynamicValue[*base.SchemaProxy, bool]{A: items},
		}
		if f.IsNullable() {
			// The array is what may be absent, not its elements: a nullable
			// array field is []string in Go, never []*string.
			out.Type = []string{"array", "null"}
		}
		e.annotate(out, f)
		return base.CreateSchemaProxy(out)

	case isRef && f.IsNullable():
		out := &base.Schema{OneOf: []*base.SchemaProxy{
			base.CreateSchemaProxyRef(schemaRef(f.Type)),
			base.CreateSchemaProxy(&base.Schema{Type: []string{"null"}}),
		}}
		e.annotate(out, f)
		return base.CreateSchemaProxy(out)

	case isRef:
		// A description beside a $ref. 2020-12 allows the sibling, and dropping
		// it would lose the only copy of the field's own words — a resource's
		// description explains the type, not this use of it.
		out := &base.Schema{}
		e.annotate(out, f)
		if out.Description == "" {
			return base.CreateSchemaProxyRef(schemaRef(f.Type))
		}
		return base.CreateSchemaProxyRefWithSchema(schemaRef(f.Type), out)

	default:
		if f.IsNullable() && len(inner.Type) == 1 {
			inner.Type = []string{inner.Type[0], "null"}
		}
		e.annotate(inner, f)
		return base.CreateSchemaProxy(inner)
	}
}

// baseSchema is the field's type before modifiers, and whether it is a
// reference to a named shape.
func (e *emitter) baseSchema(f ir.Field) (*base.Schema, bool) {
	switch f.TypeKind {
	case ir.TypeKindEnum, ir.TypeKindObject, ir.TypeKindResource:
		return &base.Schema{}, true
	}
	return primitiveSchema(f.Type), false
}

// annotate fills in everything about a field that is not its type.
func (e *emitter) annotate(s *base.Schema, f ir.Field) {
	s.Description = fieldDescription(f)
	if f.ReadOnly {
		s.ReadOnly = boolPtr(true)
	}
	if f.Format != "" && f.TypeKind == ir.TypeKindPrimitive {
		if name, ok := registeredFormats[f.Format]; ok {
			s.Format = name
		}
	}
	if f.Example != "" {
		if n := literalNode(f.Example, s.Type); n != nil {
			s.Examples = []*yaml.Node{n}
		}
	}
}

// fieldDescription is the document's own words, plus whatever this output can
// add that the document could not have said.
//
// The document's sentence comes first and unaltered: ir.Field.Description is
// the single copy of the human-readable text, and a paraphrase here is how
// three descriptions of one field start disagreeing.
func fieldDescription(f ir.Field) string {
	parts := []string{}
	if f.Description != "" {
		parts = append(parts, f.Description)
	}
	if note, ok := formatNotes[f.Format]; ok && f.TypeKind == ir.TypeKindPrimitive {
		parts = append(parts, note)
	}
	if f.Type == ir.TypeDecimal {
		parts = append(parts, decimalNote)
	}
	return strings.Join(parts, "\n\n")
}

// requestDefault renders a field's default, or nil when it must not be stated.
//
// ir.Field.Default carries two unrelated things. On a query parameter it is a
// wire literal the compiler wrote — "50", "0", "own" — and documenting it is
// the whole point. On a column-backed field it is the Postgres default
// expression, so the fixtures hold now(), false and 1, and emitting those
// verbatim would put `default: now()` in a schema.
//
// Rather than ask which kind of field this is, ask whether the value is a
// literal of the type the schema says. now() and gen_random_uuid() and
// 'draft'::lesson_status are not, so they are dropped; false and 1 are, and
// they are true of the column as well as parseable.
func requestDefault(f ir.Field, types []string) *yaml.Node {
	if f.Default == "" || f.IsArray() {
		return nil
	}
	return literalNode(f.Default, types)
}

// literalNode parses a document string into the YAML scalar its schema type
// calls for, or nil when it is not one.
func literalNode(raw string, types []string) *yaml.Node {
	kind := ""
	for _, t := range types {
		if t != "null" {
			kind = t
			break
		}
	}
	switch kind {
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil
		}
		return boolNode(b)
	case "integer":
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return nil
		}
		return scalarNode("!!int", raw)
	case "number":
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return nil
		}
		return scalarNode("!!float", raw)
	case "string":
		// A SQL string default arrives quoted, and frequently cast:
		// 'draft'::lesson_status. Anything that is not a bare word or a plain
		// quoted literal is an expression, and an expression is not a default a
		// client can send.
		if strings.ContainsAny(raw, "()") || strings.Contains(raw, "::") {
			return nil
		}
		return scalarNode("!!str", strings.Trim(raw, "'"))
	}
	return nil
}

func scalarNode(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func boolNode(b bool) *yaml.Node { return scalarNode("!!bool", strconv.FormatBool(b)) }

func boolPtr(b bool) *bool { return &b }
