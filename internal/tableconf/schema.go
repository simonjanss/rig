package tableconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaID is the identifier the emitted schema publishes itself under.
const SchemaID = "https://rig.dev/schema/table.v1.json"

// Schema returns the JSON Schema for a table configuration file, indented and
// ready to write to disk.
//
// Editors consume this for completion and inline errors, and the loader
// validates against the very same document. One schema, so what your editor
// accepts and what rig accepts cannot drift apart.
func Schema() ([]byte, error) {
	r := &jsonschema.Reflector{
		// Unknown keys are rejected. A typo in a key would otherwise be
		// silently ignored, which is the worst possible outcome: the file looks
		// configured and behaves as though it is not.
		AllowAdditionalProperties: false,
		// Inline the root so the top level reads as a table file rather than as
		// a $ref into $defs.
		ExpandedStruct: true,
		DoNotReference: false,
	}

	s := r.Reflect(&File{})
	s.ID = SchemaID
	s.Title = "rig table configuration"
	s.Description = "Configuration for one table: documentation, exposed operations, " +
		"field naming, and any endpoints beyond the generated CRUD set."

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode table schema: %w", err)
	}
	return buf.Bytes(), nil
}

var (
	compiledOnce sync.Once
	compiled     *validator.Schema
	compiledErr  error
)

// compiledSchema returns the validator built from [Schema]. Compiling is
// deferred and cached: it costs a few milliseconds and most commands validate
// many files.
func compiledSchema() (*validator.Schema, error) {
	compiledOnce.Do(func() {
		raw, err := Schema()
		if err != nil {
			compiledErr = err
			return
		}

		doc, err := validator.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			compiledErr = fmt.Errorf("parse table schema: %w", err)
			return
		}

		c := validator.NewCompiler()
		if err := c.AddResource(SchemaID, doc); err != nil {
			compiledErr = fmt.Errorf("add table schema: %w", err)
			return
		}

		compiled, err = c.Compile(SchemaID)
		if err != nil {
			compiledErr = fmt.Errorf("compile table schema: %w", err)
		}
	})
	return compiled, compiledErr
}
