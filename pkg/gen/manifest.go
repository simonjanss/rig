package gen

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// ManifestName is where the manifest lives, relative to the project root.
const ManifestName = ".rig/manifest.json"

// Manifest records what rig wrote, so that a later run can tell its own output
// from someone else's edits.
//
// It is not a cache. Generation always runs; the manifest exists to answer two
// questions a cache would not: is this file still claimed by a generator, and
// has anyone changed it since rig wrote it.
type Manifest struct {
	// Version is the manifest format, not the tool's.
	Version int `json:"version"`
	// IRHash identifies the document the recorded files were generated from.
	IRHash string `json:"ir_hash,omitempty"`
	// Generators maps a generator name to the version that last ran.
	Generators map[string]string `json:"generators,omitempty"`
	// Files is every file rig wrote, ordered by path.
	Files []Entry `json:"files"`
}

// Entry is one recorded file.
type Entry struct {
	// Path is relative to the project root, in slash form, so a manifest
	// committed on one machine reads correctly on another.
	Path      string `json:"path"`
	Generator string `json:"generator"`
	Mode      string `json:"mode"`
	SHA256    string `json:"sha256"`
}

const manifestVersion = 1

// NewManifest builds an empty manifest.
func NewManifest() *Manifest {
	return &Manifest{Version: manifestVersion, Generators: map[string]string{}}
}

// LoadManifest reads the manifest for a project. A missing manifest is not an
// error: the first run has nothing to remember.
func LoadManifest(root string) (*Manifest, error) {
	path := filepath.Join(root, filepath.FromSlash(ManifestName))

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewManifest(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		// A corrupt manifest is recoverable: the worst outcome is that rig
		// forgets what it wrote and reports a few files as new.
		return NewManifest(), nil
	}
	if m.Generators == nil {
		m.Generators = map[string]string{}
	}
	return &m, nil
}

// Save writes the manifest.
func (m *Manifest) Save(root string) error {
	path := filepath.Join(root, filepath.FromSlash(ManifestName))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	m.Version = manifestVersion
	slices.SortFunc(m.Files, func(a, b Entry) int { return cmp.Compare(a.Path, b.Path) })

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// Lookup returns the recorded entry for a path relative to the root.
func (m *Manifest) Lookup(rel string) (Entry, bool) {
	for _, e := range m.Files {
		if e.Path == rel {
			return e, true
		}
	}
	return Entry{}, false
}

// Sum is the digest the manifest records for a file's content.
func Sum(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}
