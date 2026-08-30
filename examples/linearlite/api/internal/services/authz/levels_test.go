package authz_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/examples/linearlite/internal/generated/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/services/authz"
)

// What somebody Basic may do, pinned against the whole derived catalogue.
//
// [authz.Levels] is a function of suffixes rather than a list, which is the
// point of it — a table added to the schema lands in all three levels without
// anybody editing that file. The cost is that a key whose suffix does not match
// what it means arrives in a level nobody chose it for, and the two ways that
// has happened are worth keeping shut.
//
// A `.write` on a table nobody owns a row of is one: while rig's own
// configuration for rig_account carried Update, `rig_account.write` matched the
// suffix rule and every member of a tenant was one PATCH away from Owner. A
// `.read` on the engine's own tables is the other, and it is a read of every
// notification in the tenant rather than of anybody's own.
func TestWhatBasicHolds(t *testing.T) {
	all := api.PermissionKeys()
	basic := authz.Levels(all)[string(account.RoleBasic)]

	// Nothing outside the catalogue, so a renamed key cannot survive here as a
	// grant that checks nothing.
	for _, key := range basic {
		if key == "apikey.own" || slices.Contains(all, key) {
			continue
		}
		t.Errorf("Basic holds %q, which rig does not derive", key)
	}

	for _, key := range []string{
		authz.PermissionReadNotifications,
		authz.PermissionReadDeliveries,
		authz.PermissionReadAllDevices,
	} {
		if !slices.Contains(all, key) {
			t.Errorf("%q is no longer derived; this test is checking a name nothing uses", key)
		}
		if slices.Contains(basic, key) {
			t.Errorf("Basic holds %q, which reads the whole tenant's rows", key)
		}
	}

	// And no write on a table whose rows are not the caller's. The three
	// owner-scoped notification tables are the exception and are named in the
	// file under test; everything else that ends in .write or .delete is the
	// board's, and the board's writes are Basic's on purpose.
	ownerScoped := []string{
		"rig_notification_device", "rig_notification_setting", "rig_notification_recipient",
	}
	for _, key := range basic {
		table, op, ok := strings.Cut(key, ".")
		if !ok || (op != "write" && op != "delete") {
			continue
		}
		if !strings.HasPrefix(table, "rig_") || slices.Contains(ownerScoped, table) {
			continue
		}
		t.Errorf("Basic holds %q on a table rig owns and nothing narrows to the caller", key)
	}
}
