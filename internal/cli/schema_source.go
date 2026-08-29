package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/ir"
)

// resolveSchema finds the schema to compile.
//
// A dump named explicitly wins, because that is the whole point of naming it:
// it lets the compiler be exercised against a schema captured earlier, with no
// database in the loop. Otherwise rig migrates and reads the real thing, which
// is the only way to be sure the document describes the system the migrations
// actually produce.
func (e *env) resolveSchema(ctx context.Context, p *project.Project, schemaPath string) (ir.Schema, error) {
	if schemaPath != "" {
		return readSchemaFile(schemaPath)
	}
	return e.readSchema(ctx, p)
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
