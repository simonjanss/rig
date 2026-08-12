package readopt_test

import (
	"testing"

	"github.com/simonjanss/rig/runtime/readopt"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := readopt.Apply(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The defaults are the safe ones, applied without being asked for.
	if cfg.IncludeDeleted || cfg.OnlyDeleted || cfg.IncludeSnapshots ||
		cfg.SkipTenantScope || cfg.SkipOwnerScope {
		t.Errorf("defaults should exclude everything: %+v", cfg)
	}
}

func TestOptions(t *testing.T) {
	t.Parallel()

	cfg, err := readopt.Apply([]readopt.Option{
		readopt.WithDeleted(), readopt.WithSnapshots(),
		readopt.WithoutTenantScope(), readopt.WithoutOwnerScope(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IncludeDeleted || !cfg.IncludeSnapshots || !cfg.SkipTenantScope || !cfg.SkipOwnerScope {
		t.Errorf("options were not applied: %+v", cfg)
	}
}

// Asking for deleted rows and only-deleted rows at once means the caller has
// confused themselves, and quietly picking one would hide that.
func TestContradictoryOptions(t *testing.T) {
	t.Parallel()

	_, err := readopt.Apply([]readopt.Option{readopt.WithDeleted(), readopt.WithOnlyDeleted()})
	if err == nil {
		t.Fatal("contradictory options should be an error")
	}
}
