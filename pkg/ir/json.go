package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Marshal encodes a document canonically: two-space indent, struct fields in
// declaration order, no HTML escaping. The same document always produces the
// same bytes, which is what lets the IR be committed and diffed.
func Marshal(d *Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("marshal ir document: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes a document and indexes it.
//
// An unknown field is an error rather than a silent drop: reading a document
// written by a newer rig should fail loudly, not half-succeed.
func Unmarshal(b []byte) (*Document, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	var d Document
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("unmarshal ir document: %w", err)
	}
	if d.IRVersion != CurrentVersion {
		return nil, fmt.Errorf("unsupported ir version %d, this rig speaks %d", d.IRVersion, CurrentVersion)
	}
	d.Reindex()
	return &d, nil
}

// Hash returns the SHA-256 of the document's canonical encoding, prefixed with
// the algorithm. It identifies the document's content, not the run that
// produced it, so an unchanged schema and configuration always hash the same.
//
// [API.Revision] is cleared before hashing, and that is not a detail: the
// revision is derived from this hash, so a hash that saw it would move every
// time it was set, which would move the revision, which would move the hash. It
// is cleared here rather than by each caller because there is no caller for whom
// the other behavior is right.
//
// [API.EmbeddedFoundation] is cleared for a different reason, and the two are
// worth telling apart. It is in the document because a generator has to know
// where rig's own migrations come from — but no client can tell, and the revision
// is what a client reads to decide whether it is talking to an API it was built
// against. Moving a project's DDL from its own directory into the modules that
// own it changes nothing anybody could observe over HTTP, so it must not spend a
// revision saying otherwise.
func (d *Document) Hash() (string, error) {
	unstamped := *d
	unstamped.API.Revision = ""
	unstamped.API.EmbeddedFoundation = false

	b, err := Marshal(&unstamped)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
