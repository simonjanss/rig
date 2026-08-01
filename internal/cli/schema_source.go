package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/ir"
)

// errNoSchemaSource explains the one thing M0 cannot do yet.
var errNoSchemaSource = errors.New(
	"no schema available: pass --schema with a dump written by `rig ir --schema-only`.\n" +
		"Reading the schema from a live database is not wired up yet")

// resolveSchema finds the schema to compile.
//
// Introspection is the only impure part of the pipeline and lands in the next
// milestone. Until then a dump stands in — which is not a workaround so much as
// the same seam the compiler's own tests use, and will keep using afterwards.
func resolveSchema(p *project.Project, schemaPath string) (ir.Schema, error) {
	if schemaPath != "" {
		return readSchemaFile(schemaPath)
	}

	// A dump committed alongside the project is picked up automatically, which
	// makes the whole command line usable before introspection exists.
	if def := p.Path(".rig/schema.json"); fileExists(def) {
		return readSchemaFile(def)
	}

	return ir.Schema{}, errNoSchemaSource
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// marshalIndent encodes a value the same way the IR is encoded, so a schema
// dump and a document read alike.
func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
