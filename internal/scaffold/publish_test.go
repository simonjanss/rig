package scaffold_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/scaffold"
)

// The first run: no publication anywhere, so this migration is the one that makes
// it, and Down may drop it.
func TestPublishCreatesThePublicationOnTheFirstRun(t *testing.T) {
	t.Parallel()

	out := scaffold.Publish(scaffold.PublishOptions{
		Create:   true,
		Add:      []string{"todo", "rig_presence"},
		Identity: []string{"todo", "rig_presence"},
	})

	for _, want := range []string{
		"CREATE PUBLICATION electric_publication_default;",
		"ALTER PUBLICATION electric_publication_default ADD TABLE todo, rig_presence;",
		"ALTER TABLE todo REPLICA IDENTITY FULL;",
		"ALTER TABLE rig_presence REPLICA IDENTITY FULL;",
		"DROP PUBLICATION electric_publication_default;",
		"-- +goose Up",
		"-- +goose Down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// The second run, which is the whole reason this is a command rather than a
// paragraph in the documentation. A table that gained a shape later is added to
// the publication that is already there — and Down must not drop that
// publication, because an earlier migration's tables are in it.
func TestPublishAddingToAnExistingPublicationDoesNotDropIt(t *testing.T) {
	t.Parallel()

	out := scaffold.Publish(scaffold.PublishOptions{
		Create:   false,
		Add:      []string{"note"},
		Identity: []string{"note"},
		Already:  []string{"todo", "rig_presence"},
	})

	if strings.Contains(out, "CREATE PUBLICATION") {
		t.Errorf("created a publication it was told already exists:\n%s", out)
	}
	if strings.Contains(out, "DROP PUBLICATION") {
		t.Errorf("Down drops a publication this migration did not create:\n%s", out)
	}
	if !strings.Contains(out, "ALTER PUBLICATION electric_publication_default DROP TABLE note;") {
		t.Errorf("Down does not undo what Up did:\n%s", out)
	}
	// What it left alone, said in the file rather than only on the terminal —
	// the file is what somebody reads six months later.
	if !strings.Contains(out, "left alone: todo, rig_presence") {
		t.Errorf("the file does not say what was already published:\n%s", out)
	}
}

// The state RIG5093 reports and RIG5090 does not: published by an earlier
// migration, and never given an identity live sync can use. Nothing to add, one
// statement to write.
func TestPublishCanSetAnIdentityWithoutPublishingAnything(t *testing.T) {
	t.Parallel()

	out := scaffold.Publish(scaffold.PublishOptions{
		Identity: []string{"todo"},
		Already:  []string{"todo"},
	})

	if strings.Contains(out, "ALTER PUBLICATION") {
		t.Errorf("touched the publication when only the identity was wrong:\n%s", out)
	}
	if !strings.Contains(out, "ALTER TABLE todo REPLICA IDENTITY FULL;") {
		t.Errorf("did not set the identity:\n%s", out)
	}
	if !strings.Contains(out, "ALTER TABLE todo REPLICA IDENTITY DEFAULT;") {
		t.Errorf("Down does not put the identity back:\n%s", out)
	}
}

// A publication a project named itself, rather than rig's default.
func TestPublishHonoursANamedPublication(t *testing.T) {
	t.Parallel()

	out := scaffold.Publish(scaffold.PublishOptions{
		Publication: "app_publication",
		Add:         []string{"todo"},
	})
	if !strings.Contains(out, "ALTER PUBLICATION app_publication ADD TABLE todo;") {
		t.Errorf("did not use the named publication:\n%s", out)
	}
	if strings.Contains(out, "electric_publication_default") {
		t.Errorf("named rig's default anyway:\n%s", out)
	}
}
