// Package revision records when a project's API surface last changed.
//
// The date is the whole point, and so is what it is not: it is not a build
// timestamp. A timestamp moves on every regeneration, so two clients built a
// month apart against an unchanged API look a month apart when they are
// identical, and the number is noise within a week. This moves only when the
// document's hash does — which is to say, only when the surface a client was
// built against actually changed.
//
// The file is committed. That is what makes `git log` a record of when the API
// moved, and what makes a clean checkout regenerate the same bytes rather than
// stamping today over a date somebody chose in March.
package revision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Name is where the record lives, relative to the project root.
//
// It sits beside .rig/manifest.json and is committed where that one is not: the
// manifest describes this machine's last run, and this describes the project.
const Name = ".rig/revision.json"

// Layout is how a revision is written. A date and nothing finer: the question
// it answers is "how many releases behind is this client", and an hour has
// never been the unit of that answer.
const Layout = "2006-01-02"

// File is the recorded pair.
//
// Both halves are needed. The hash alone cannot say when, and the date alone
// cannot say whether the surface has moved since.
type File struct {
	// Revision is the date the surface last changed, as YYYY-MM-DD.
	Revision string `json:"revision"`
	// Hash is the document hash that date belongs to.
	Hash string `json:"hash"`
}

// Load reads the record for a project.
//
// A missing file is not an error: a project that has never generated has
// nothing to remember, and the zero File resolves to today on the next run.
func Load(root string) (File, error) {
	raw, err := os.ReadFile(path(root))
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", Name, err)
	}

	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		// Unlike the manifest, a corrupt record here is not recoverable by
		// forgetting it: forgetting means stamping today over a date that is in
		// somebody's git history and in every client already built.
		return File{}, fmt.Errorf("read %s: %w", Name, err)
	}
	return f, nil
}

// Save writes the record.
func (f File) Save(root string) error {
	p := path(root)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o644)
}

// Resolve decides what the revision is now.
//
// Same hash as the recorded one, and a date to go with it, means the surface has
// not moved: the old date stands, and every generator produces the byte it
// produced yesterday. Anything else — a different hash, a record that was never
// written, a date somebody deleted — is today.
//
// It is pure and takes its clock as an argument, so the decision can be tested
// without waiting a day for the interesting case.
func Resolve(prev File, hash string, today time.Time) (File, bool) {
	if prev.Revision != "" && prev.Hash == hash {
		return prev, false
	}
	return File{Revision: today.UTC().Format(Layout), Hash: hash}, true
}

func path(root string) string { return filepath.Join(root, filepath.FromSlash(Name)) }
