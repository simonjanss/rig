package dockerdb

// The ports the Docker suites pin, in one place.
//
// Every suite starts a container on a fixed host port so a warm one can be
// reused between runs, and until this file existed each of them chose that port
// by grepping for a number nobody else seemed to be using. That works until two
// suites pick the same one, and then it fails as a connection to somebody else's
// database with somebody else's schema in it — which looks like a bug in the
// code under test rather than a collision.
//
// Adding a suite means adding a constant here first. TestPortsAreUnique is what
// makes that more than an instruction.
//
// These numbers keep two suites in one checkout apart. They do nothing about two
// checkouts, which is a different problem with a different answer — see
// isolate.go, where a port from this list becomes a request rather than a
// requirement.
//
// The examples are the exception and are listed rather than used: each one's
// port lives in its own rig.yaml and main.go, in a module that cannot import
// this one. They are named here so the numbers are still allocated from one
// list, and TestPortsAreUnique holds them to the same rule.
const (
	// PortDefault is what `rig init` writes into a new project, so it is the one
	// number here a real project sees. Not a suite's, and not to be taken by one.
	PortDefault = 55432

	// The examples, whose databases `make examples` brings up. Declared here,
	// defined in each example's own configuration.
	PortExampleTodo            = 55440
	PortExampleFantasyFootball = 55441
	PortExampleAuth            = 55442
	PortExampleAuthOAuth       = 55443

	// PortFiles is internal/filestest: uploads, the tenancy around them, and the
	// sweeper.
	PortFiles = 55494

	// PortElectricSync is the ElectricSQL container in internal/electrictest,
	// which is an HTTP port rather than a database one. It is in the same list
	// because it is the same host and the same collision.
	PortElectricSync = 55490
	// PortIntrospect is internal/introspect, reading a real catalogue.
	PortIntrospect = 55491
	// PortElectricDB is the database internal/electrictest syncs from.
	PortElectricDB = 55492
	// PortCLIEndToEnd is internal/cli's end-to-end loop.
	PortCLIEndToEnd = 55493
	// PortCLISetup and PortCLISetupOwn are internal/cli's setup-project tests.
	// Two, because one of them takes the schema over and the other does not, and
	// they run in parallel.
	PortCLISetup    = 55496
	PortCLISetupOwn = 55497
	// PortAuth is internal/authtest.
	PortAuth = 55498
)

// ports is every number above, with the name a failure should mention.
//
// It is written out rather than derived, because the point is to have one list
// that a person reads and a test checks.
var ports = map[string]int{
	"default (rig init)":           PortDefault,
	"examples/todo":                PortExampleTodo,
	"examples/fantasyfootball":     PortExampleFantasyFootball,
	"examples/auth":                PortExampleAuth,
	"examples/auth_oauth":          PortExampleAuthOAuth,
	"internal/filestest":           PortFiles,
	"internal/electrictest (sync)": PortElectricSync,
	"internal/introspect":          PortIntrospect,
	"internal/electrictest (db)":   PortElectricDB,
	"internal/cli (e2e)":           PortCLIEndToEnd,
	"internal/cli (setup)":         PortCLISetup,
	"internal/cli (setup, own)":    PortCLISetupOwn,
	"internal/authtest":            PortAuth,
}
