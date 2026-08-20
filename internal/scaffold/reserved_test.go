package scaffold_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/scaffold"
)

// Every foundation table reserves the name it projects to. This is what makes
// "adding a part reserves its names" a fact rather than something somebody has
// to remember: a table with no configuration would slip through the map, and a
// project could then take the name the day that part was exposed.
func TestEveryFoundationTableReservesItsResource(t *testing.T) {
	t.Parallel()

	reserved := scaffold.ReservedResources()

	claimed := make(map[string]string, len(reserved))
	for resource, table := range reserved {
		if prev, dup := claimed[table]; dup {
			t.Errorf("table %s reserves both %s and %s", table, prev, resource)
		}
		claimed[table] = resource
	}

	for _, table := range scaffold.Tables() {
		if _, ok := claimed[table]; !ok {
			t.Errorf("table %s reserves no resource name; give it a configuration", table)
		}
	}
}

// Nothing is reserved on behalf of a table that is not rig's to create. A name
// held for an ordinary table would be rig taking something it does not own.
func TestReservedNamesBelongToRigTables(t *testing.T) {
	t.Parallel()

	for resource, table := range scaffold.ReservedResources() {
		if !strings.HasPrefix(table, scaffold.TablePrefix) {
			t.Errorf("%s is reserved for %s, which is not rig's to create", resource, table)
		}
	}
}

// Every part reserves its names by existing, which is the property that makes
// the derived set worth deriving. Naming the notification part specifically
// because it is the one that arrived after the rule did: it reserved five names
// without anybody editing a list.
func TestANewPartReservesItsNames(t *testing.T) {
	t.Parallel()

	reserved := scaffold.ReservedResources()
	for _, want := range []string{
		"Notification", "NotificationRecipient", "NotificationDevice",
		"NotificationSetting", "NotificationDelivery",
	} {
		table, ok := reserved[want]
		if !ok {
			t.Errorf("%s is not reserved", want)
			continue
		}
		if !slices.Contains(scaffold.PartTables(scaffold.PartNotifications), table) {
			t.Errorf("%s is reserved for %s, which is not a notification table", want, table)
		}
	}
}
