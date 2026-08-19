package readopt_test

import (
	"fmt"

	"github.com/simonjanss/rig/runtime/readopt"
)

// A generated read excludes soft-deleted rows, excludes snapshots and scopes to
// the caller's tenant without being asked. These are how a caller says
// otherwise, and Apply is what a repository resolves them with.
func ExampleApply() {
	cfg, err := readopt.Apply([]readopt.Option{readopt.WithDeleted()})
	if err != nil {
		panic(err)
	}

	fmt.Println("deleted rows included:", cfg.IncludeDeleted)
	fmt.Println("snapshots included:   ", cfg.IncludeSnapshots)

	// Output:
	// deleted rows included: true
	// snapshots included:    false
}

// Contradictory options are an error rather than a precedence rule. Asking for
// deleted rows and only-deleted rows at once means the caller has confused
// themselves, and quietly picking one would hide that.
func ExampleApply_contradiction() {
	_, err := readopt.Apply([]readopt.Option{
		readopt.WithDeleted(),
		readopt.WithOnlyDeleted(),
	})

	fmt.Println(err)

	// Output:
	// BadRequest: WithDeleted and WithOnlyDeleted contradict each other
}

// No options is the narrow read, which is the direction that makes forgetting
// one safe: a handler that says nothing returns too little rather than too much.
func ExampleApply_defaults() {
	cfg, err := readopt.Apply(nil)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%+v\n", cfg)

	// Output:
	// {IncludeDeleted:false OnlyDeleted:false IncludeSnapshots:false SkipTenantScope:false SkipOwnerScope:false}
}
