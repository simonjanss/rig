package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/pkg/ir"
)

// doc builds the smallest document publicationPlan reads: the resources that
// stream, and the schema facts introspection puts on each table.
func doc(pubs []ir.Publication, tables ...ir.Table) *ir.Document {
	d := &ir.Document{}
	d.Schema.Replication = &ir.Replication{WALLevel: "logical", Publications: pubs}
	d.Schema.Tables = tables
	for _, t := range tables {
		d.API.Resources = append(d.API.Resources, ir.Resource{
			Electric: &ir.ElectricEndpoint{},
			Storage:  &ir.ResourceStorage{Table: t.Name},
		})
	}
	return d
}

// The first run against a database nothing has published. Everything streams,
// nothing is carried, so the migration creates the publication and takes the lot —
// and creating it is what makes the migration own it, which is what the deployment
// rests on.
func TestThePlanForAFreshDatabaseCreatesThePublication(t *testing.T) {
	t.Parallel()

	d := doc(nil,
		ir.Table{Name: "todo", ReplicaIdentity: ir.ReplicaIdentityDefault},
		ir.Table{Name: "rig_presence", ReplicaIdentity: ir.ReplicaIdentityDefault},
	)

	plan, err := publicationPlan(d, streamingTables(d))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Create {
		t.Error("did not create a publication against a database with none")
	}
	if plan.Publication != scaffold.DefaultPublication {
		t.Errorf("Publication = %q, want the one the sync service reads", plan.Publication)
	}
	if want := []string{"rig_presence", "todo"}; !slices.Equal(plan.Add, want) {
		t.Errorf("Add = %v, want %v", plan.Add, want)
	}
	if want := []string{"rig_presence", "todo"}; !slices.Equal(plan.Identity, want) {
		t.Errorf("Identity = %v, want %v", plan.Identity, want)
	}
	if len(plan.Already) != 0 {
		t.Errorf("Already = %v, want none", plan.Already)
	}
}

// The case the whole command is for: a table gained a shape after the first
// migration was applied. Only that table goes in the new file, and it goes into the
// publication that is already there rather than a second one.
func TestThePlanAddsOnlyTheTableThatIsNew(t *testing.T) {
	t.Parallel()

	d := doc([]ir.Publication{{Name: scaffold.DefaultPublication, Owned: true}},
		ir.Table{
			Name:            "todo",
			Publications:    []string{scaffold.DefaultPublication},
			ReplicaIdentity: ir.ReplicaIdentityFull,
		},
		ir.Table{Name: "note", ReplicaIdentity: ir.ReplicaIdentityDefault},
	)

	plan, err := publicationPlan(d, streamingTables(d))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Create {
		t.Error("created a publication that is already there")
	}
	if want := []string{"note"}; !slices.Equal(plan.Add, want) {
		t.Errorf("Add = %v, want %v", plan.Add, want)
	}
	if want := []string{"note"}; !slices.Equal(plan.Identity, want) {
		t.Errorf("Identity = %v, want %v", plan.Identity, want)
	}
	if want := []string{"todo"}; !slices.Equal(plan.Already, want) {
		t.Errorf("Already = %v, want %v", plan.Already, want)
	}
}

// A publication under a name of the project's own is not an answer, because the sync
// service never reads it. This is the state every rig project written before the
// ownership question was understood is in — `rig_publication`, full of the right
// tables, consulted by nothing — and the plan has to treat those tables as
// unpublished rather than as done.
func TestThePlanIgnoresAPublicationTheSyncServiceNeverReads(t *testing.T) {
	t.Parallel()

	d := doc([]ir.Publication{{Name: "rig_publication", Owned: true}},
		ir.Table{
			Name:            "todo",
			Publications:    []string{"rig_publication"},
			ReplicaIdentity: ir.ReplicaIdentityFull,
		},
	)

	plan, err := publicationPlan(d, streamingTables(d))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Create {
		t.Error("took a publication of the project's own as the one to add to")
	}
	if plan.Publication != scaffold.DefaultPublication {
		t.Errorf("Publication = %q", plan.Publication)
	}
	if want := []string{"todo"}; !slices.Equal(plan.Add, want) {
		t.Errorf("Add = %v, want %v", plan.Add, want)
	}
}

// The one state this cannot repair, and the reason it refuses rather than writing
// SQL that will fail. The sync service created its publication before any migration
// did — which is what happens on a database that was already running before the
// project had this migration — and nothing here can transfer ownership.
func TestThePlanRefusesAPublicationTheSyncServiceOwns(t *testing.T) {
	t.Parallel()

	d := doc([]ir.Publication{{Name: scaffold.DefaultPublication, Owned: false}},
		ir.Table{
			Name:            "todo",
			Publications:    []string{scaffold.DefaultPublication},
			ReplicaIdentity: ir.ReplicaIdentityFull,
		},
	)

	_, err := publicationPlan(d, streamingTables(d))
	if err == nil {
		t.Fatal("planned an ALTER PUBLICATION against a publication it does not own")
	}
	// Both ways out have to be in the message. `rig db reset` is not advice anybody
	// can take on a managed database, which is where this state actually turns up —
	// so the ownership transfer has to be there too, or the reader is stuck.
	for _, want := range []string{"ALTER PUBLICATION", "OWNER TO", "rig db reset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// Published already, and the identity never set. Nothing to add, one statement to
// write — which is the half RIG5090 cannot see and RIG5093 reports.
func TestThePlanFixesAnIdentityOnAnAlreadyPublishedTable(t *testing.T) {
	t.Parallel()

	d := doc([]ir.Publication{{Name: scaffold.DefaultPublication, Owned: true}},
		ir.Table{
			Name:            "todo",
			Publications:    []string{scaffold.DefaultPublication},
			ReplicaIdentity: ir.ReplicaIdentityDefault,
		},
	)

	plan, err := publicationPlan(d, streamingTables(d))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 0 {
		t.Errorf("Add = %v, want none", plan.Add)
	}
	if want := []string{"todo"}; !slices.Equal(plan.Identity, want) {
		t.Errorf("Identity = %v, want %v", plan.Identity, want)
	}
}

// Nothing missing, so nothing is written. A command that produced an empty migration
// every time it ran would fill a directory with files that apply nothing.
func TestThePlanIsEmptyWhenEverythingIsAlreadyRight(t *testing.T) {
	t.Parallel()

	d := doc([]ir.Publication{{Name: scaffold.DefaultPublication, Owned: true}},
		ir.Table{
			Name:            "todo",
			Publications:    []string{scaffold.DefaultPublication},
			ReplicaIdentity: ir.ReplicaIdentityFull,
		},
	)

	plan, err := publicationPlan(d, streamingTables(d))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 0 || len(plan.Identity) != 0 {
		t.Errorf("Add = %v, Identity = %v, want both empty", plan.Add, plan.Identity)
	}
}

// A schema nobody read the replication facts off cannot be compared against, and
// guessing would write ALTER PUBLICATION for tables that may already be in one.
func TestThePlanRefusesASchemaWithNoReplicationFacts(t *testing.T) {
	t.Parallel()

	d := doc(nil, ir.Table{Name: "todo"})
	d.Schema.Replication = nil

	if _, err := publicationPlan(d, streamingTables(d)); err == nil {
		t.Fatal("planned against a schema with no replication facts")
	}
}
