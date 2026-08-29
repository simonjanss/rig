package yamlconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/message"

	"github.com/simonjanss/rig/internal/diag"
)

// Format describes one configuration file format: the Go struct that defines
// it, and the identity its JSON Schema publishes itself under.
//
// The schema is generated once and reused for two purposes — written to disk
// for editors, and compiled as the validator. One schema, so what an editor
// accepts and what rig accepts cannot diverge.
type Format struct {
	// ID is the schema's $id, for example "https://rig.dev/schema/table.v1.json".
	ID string
	// Title and Description document the format in the emitted schema.
	Title       string
	Description string
	// New returns a fresh zero value of the format's root struct. It is called
	// both to reflect the schema and to decode into.
	New func() any

	once     sync.Once
	rawSch   []byte
	compiled *validator.Schema
	initErr  error
}

// Schema returns the JSON Schema for the format, indented for writing to disk.
func (f *Format) Schema() ([]byte, error) {
	f.init()
	return f.rawSch, f.initErr
}

func (f *Format) init() {
	f.once.Do(func() {
		r := &jsonschema.Reflector{
			// Unknown keys are rejected. A typo would otherwise be silently
			// ignored, which is the worst outcome: the file looks configured
			// and behaves as though it is not.
			AllowAdditionalProperties: false,
			// Inline the root so the top level reads as the format itself
			// rather than as a $ref into $defs.
			ExpandedStruct: true,
		}

		s := r.Reflect(f.New())
		s.ID = jsonschema.ID(f.ID)
		s.Title = f.Title
		s.Description = f.Description

		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			f.initErr = fmt.Errorf("encode %s schema: %w", f.Title, err)
			return
		}
		f.rawSch = buf.Bytes()

		doc, err := validator.UnmarshalJSON(bytes.NewReader(f.rawSch))
		if err != nil {
			f.initErr = fmt.Errorf("parse %s schema: %w", f.Title, err)
			return
		}

		c := validator.NewCompiler()
		if err := c.AddResource(f.ID, doc); err != nil {
			f.initErr = fmt.Errorf("add %s schema: %w", f.Title, err)
			return
		}
		if f.compiled, err = c.Compile(f.ID); err != nil {
			f.initErr = fmt.Errorf("compile %s schema: %w", f.Title, err)
		}
	})
}

// Decode parses, validates, and decodes a configuration file into target.
//
// It returns the anchor index whenever the YAML parsed, even if validation
// failed, so a caller can still report positions for problems it finds itself.
// ok is false when target was not populated.
func (f *Format) Decode(path string, data []byte, target any) (ix *Index, ok bool, diags diag.List) {
	astFile, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, errorAnchor(path, err), "%s", errorMessage(err))
		return nil, false, diags
	}

	root := rootNode(astFile)
	ix = BuildIndex(path, root)

	// An empty document is a file with nothing in it; say so, rather than
	// letting it fail schema validation with a confusing type error.
	if root == nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"configuration file is empty")
		return ix, false, diags
	}

	f.init()
	if f.initErr != nil {
		diags.Add(diag.CodeInternal, diag.Anchor{File: path}, "%v", f.initErr)
		return ix, false, diags
	}

	// Validate through JSON so the instance uses the exact value types the
	// validator expects, with no YAML-specific shapes left over.
	jsonBytes, err := yaml.YAMLToJSON(data)
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"cannot convert configuration to JSON for validation: %v", err)
		return ix, false, diags
	}

	instance, err := validator.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		diags.Add(diag.CodeConfigSyntax, diag.Anchor{File: path, Line: 1, Column: 1},
			"cannot read configuration for validation: %v", err)
		return ix, false, diags
	}

	if err := f.compiled.Validate(instance); err != nil {
		var verr *validator.ValidationError
		if errors.As(err, &verr) {
			for _, leaf := range leafErrors(verr) {
				for _, p := range anchorPaths(leaf) {
					diags.Add(diag.CodeConfigInvalid, ix.At(p), "%s", schemaMessage(leaf))
				}
			}
		} else {
			diags.Add(diag.CodeConfigInvalid, diag.Anchor{File: path, Line: 1, Column: 1}, "%v", err)
		}
		return ix, false, diags
	}

	if err := yaml.Unmarshal(data, target); err != nil {
		diags.Add(diag.CodeConfigSyntax, errorAnchor(path, err), "%s", errorMessage(err))
		return ix, false, diags
	}

	return ix, true, diags
}

func rootNode(f *ast.File) ast.Node {
	if f == nil {
		return nil
	}
	for _, doc := range f.Docs {
		if doc != nil && doc.Body != nil {
			return doc.Body
		}
	}
	return nil
}

// leafErrors flattens a validation error tree down to the errors that actually
// describe a problem. Interior nodes say things like "does not validate against
// #/$defs/Column", which repeats what the leaf says with less precision.
func leafErrors(e *validator.ValidationError) []*validator.ValidationError {
	if len(e.Causes) == 0 {
		return []*validator.ValidationError{e}
	}
	var out []*validator.ValidationError
	for _, c := range e.Causes {
		out = append(out, leafErrors(c)...)
	}
	return out
}

var englishPrinter = message.NewPrinter(message.MatchLanguage("en"))

// anchorPaths returns the paths a validation error should be reported at.
//
// Most errors describe the value at their instance location. An unknown key is
// the exception: it is reported against the enclosing object, and anchoring
// there would point a reader at the top of a block rather than at the word they
// mistyped. Naming the offending key turns "something in this table is wrong"
// into a cursor on the exact line.
func anchorPaths(e *validator.ValidationError) []string {
	base := strings.Join(e.InstanceLocation, ".")

	if k, isExtra := e.ErrorKind.(*kind.AdditionalProperties); isExtra {
		paths := make([]string, 0, len(k.Properties))
		for _, p := range k.Properties {
			paths = append(paths, Join(base, p))
		}
		return paths
	}
	// A missing key has no position of its own, so the enclosing object is the
	// most precise place there is.
	return []string{base}
}

// schemaMessage renders a validation error. The instance path is deliberately
// left out: the anchor already points at it, and repeating "operations.1" in
// the text adds an array index the reader does not need.
func schemaMessage(e *validator.ValidationError) string {
	return e.ErrorKind.LocalizedString(englishPrinter)
}

// errorAnchor recovers the position from a goccy parse or decode error. goccy
// formats positions into the message rather than exposing them, so the line is
// read back out of the rendered error.
func errorAnchor(path string, err error) diag.Anchor {
	a := diag.Anchor{File: path, Line: 1, Column: 1}
	var line, col int
	if n, _ := fmt.Sscanf(yaml.FormatError(err, false, false), "[%d:%d]", &line, &col); n == 2 {
		a.Line, a.Column = line, col
	}
	return a
}

func errorMessage(err error) string {
	msg := yaml.FormatError(err, false, false)
	// Drop the leading "[line:col] " that goccy prepends: the anchor already
	// carries the position, and repeating it reads as noise.
	if i := strings.Index(msg, "] "); i > 0 && strings.HasPrefix(msg, "[") {
		msg = msg[i+2:]
	}
	return strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
}
