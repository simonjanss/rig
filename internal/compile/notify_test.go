package compile_test

import (
	"path/filepath"
	"testing"

	"github.com/simonjanss/rig/pkg/ir"
)

// TestTheDispatchersNumbersAreNotInTheRevision, and what the API looks like is.
//
// The same argument the sweeper's numbers are cleared on, arrived at later and
// for a worse reason: these had been in the revision since the block shipped, and
// it took changing one of them to notice. Nothing answers a claim_ttl or any of
// the retry arithmetic to anybody — no route in notify/notifyhttp carries them,
// no generated client reads them, and the OpenAPI document mentions notifications
// only to say its endpoints are not described there. A project retuning its
// dispatcher was telling every client it was built against something older than
// the server, over a change none of them could see.
func TestTheDispatchersNumbersAreNotInTheRevision(t *testing.T) {
	t.Parallel()

	base := func() *ir.Document {
		doc, diags := compileFixture(t, filepath.Join("testdata", "notify"))
		if hasErrors(diags) {
			t.Fatalf("the notify fixture does not compile:\n%s", renderDiagnostics(diags))
		}
		return doc
	}

	before := base()
	want, err := before.Hash()
	if err != nil {
		t.Fatal(err)
	}

	// Invisible to every client: the hash must not move.
	for _, tc := range []struct {
		name  string
		apply func(*ir.Notifications)
	}{
		{"the claim lease", func(n *ir.Notifications) { n.ClaimTTLSeconds = 999 }},
		{"the send timeout", func(n *ir.Notifications) { n.SendTimeoutSeconds = 999 }},
		{"the attempt cap", func(n *ir.Notifications) { n.MaxAttempts = 999 }},
		{"the backoff base", func(n *ir.Notifications) { n.BackoffBaseSeconds = 999 }},
		{"the backoff ceiling", func(n *ir.Notifications) { n.BackoffCapSeconds = 999 }},
		{"the retention window", func(n *ir.Notifications) { n.RetentionSeconds = 999 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := base()
			tc.apply(doc.API.Notifications)
			got, err := doc.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("changing %s moved the revision, and no client can tell it "+
					"changed", tc.name)
			}
		})
	}

	// What the API is, rather than how the dispatcher runs: the hash has to move.
	for _, tc := range []struct {
		name  string
		apply func(*ir.Notifications)
	}{
		// Whether the routes exist at all.
		{"turning the inbox off", func(n *ir.Notifications) { n.Enabled = false }},
		// Whether the inbox is also projected as a resource, with a filter
		// grammar and a generated client.
		{"projecting it as a resource", func(n *ir.Notifications) { n.Expose = !n.Expose }},
		// The judgement call in the set. No response carries it, but it decides
		// what an account with no setting receives — so a settings page built
		// when the default was Immediate behaves differently against Weekly.
		{"the default digest", func(n *ir.Notifications) { n.DefaultDigest = "Weekly" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := base()
			tc.apply(doc.API.Notifications)
			got, err := doc.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Errorf("changing %s did not move the revision, and it changes what "+
					"the API is", tc.name)
			}
		})
	}
}

// The second nested copy Hash takes, and the second chance to get it wrong: a
// shallow copy of the document shares this pointer, so clearing six fields
// without copying the struct first would erase the caller's dispatcher tuning
// every time somebody asked for a hash.
func TestHashingDoesNotEraseTheDispatchersNumbers(t *testing.T) {
	t.Parallel()

	doc, _ := compileFixture(t, filepath.Join("testdata", "notify"))
	before := *doc.API.Notifications
	if before.ClaimTTLSeconds == 0 || before.MaxAttempts == 0 || before.BackoffCapSeconds == 0 {
		t.Fatal("the fixture resolves no dispatcher numbers, so this test proves nothing")
	}

	if _, err := doc.Hash(); err != nil {
		t.Fatal(err)
	}
	if got := *doc.API.Notifications; got != before {
		t.Errorf("hashing changed the block: %+v became %+v", before, got)
	}
}
