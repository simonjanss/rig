package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
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
//
// [Document.Tool] is cleared for that reason, and it is the one field here that
// says nothing about the project at all: it is the build of rig that produced
// the document. Left in, every release of rig would move every project's
// revision — upgrading the generator would tell every client the API had
// changed, on a day the API did not. The version belongs in the document, which
// is why it is still written; it does not belong in what the document is
// compared by.
//
// [API.Monitoring] is cleared for that same reason. rig's own page is mounted
// beside the API rather than in it: it appears in no specification, no
// generated client calls it, and a caller cannot tell whether it is there. A
// project that turned the page on and spent a revision on it would be telling
// every client it was built against something older than the server, over a
// change none of them can see.
//
// [API.Cache] is cleared for that reason too, and it is the clearest case of it:
// whether a replica answered out of memory or out of the database is not
// something a client can observe at all. The generated clients are told the
// backstop lifetime all the same — see rigclient.AuthProfile — because a
// document consumer should be able to read how long a lost invalidation could
// go unnoticed. Being told a number is not the same as behaving differently for
// it, and only the second one is what a revision means.
//
// [Resource.Cached] is cleared for the same reason as [API.Cache], and it has to
// be: it is the per-table half of the same switch, so leaving it in would let
// turning a cache on move a revision that turning the block on does not. It is a
// slice rather than a field, so clearing it copies — the caller's document must
// not come back with its opt-ins erased.
//
// [API.Notifications] is cleared in part, and it is the second field here to be
// — the dispatcher's numbers go, and the three that say what the API looks like
// stay. Nothing answers a claim_ttl, a send_timeout or any of the retry
// arithmetic to anybody: no route in notify/notifyhttp carries them, no
// generated client reads them, and the OpenAPI document mentions notifications
// only to say its endpoints are not described there. So they are cleared for
// Monitoring's reason, and it took until they were being changed to notice they
// had never been — a project that retuned its dispatcher was spending a revision
// telling every client it was built against something older than the server.
//
// Enabled and Expose stay: the first decides whether the routes exist at all and
// the second projects a resource. DefaultDigest stays too, and it is the only
// judgement call in the set. No response carries it, but it decides what an
// account with no setting actually receives, so a settings page built when the
// default was Immediate behaves differently against Weekly — which is the thing
// a revision is for.
//
// [Presence] is cleared in *part*, and it was the first field here to be. Two of
// its numbers are answered to the browser on every heartbeat, so a client built
// when the TTL was a minute behaves differently against twenty seconds — those
// stay, because that is exactly what a revision is for. The sweeper's interval
// and its grace period are housekeeping: no response carries them and no caller
// can tell what they are, so they are cleared for the reason Monitoring is.
func (d *Document) Hash() (string, error) {
	unstamped := *d
	unstamped.Tool = ""
	unstamped.API.Revision = ""
	unstamped.API.EmbeddedFoundation = false
	unstamped.API.Monitoring = nil
	unstamped.API.Cache = nil
	if slices.ContainsFunc(unstamped.API.Resources, func(r Resource) bool { return r.Cached }) {
		// A copy for the reason the two below take one: the shallow copy above
		// shares the backing array, and clearing in place would reach the
		// caller's document.
		plain := slices.Clone(unstamped.API.Resources)
		for i := range plain {
			plain[i].Cached = false
		}
		unstamped.API.Resources = plain
	}
	if n := unstamped.API.Notifications; n != nil {
		// A copy, for the reason the one below takes one: the shallow copy above
		// shares the pointer, and the caller's document must not come back with
		// its dispatcher tuning erased.
		visible := *n
		visible.ClaimTTLSeconds = 0
		visible.SendTimeoutSeconds = 0
		visible.MaxAttempts = 0
		visible.BackoffBaseSeconds = 0
		visible.BackoffCapSeconds = 0
		visible.RetentionSeconds = 0
		unstamped.API.Notifications = &visible
	}
	if p := unstamped.API.Presence; p != nil {
		// A copy, because the shallow copy above shares the pointer and the
		// caller's document must not come back with its sweep interval erased.
		visible := *p
		visible.SweepSeconds = 0
		visible.GraceSeconds = 0
		unstamped.API.Presence = &visible
	}

	b, err := Marshal(&unstamped)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
