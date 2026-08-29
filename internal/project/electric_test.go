package project_test

import (
	"slices"
	"testing"

	"github.com/simonjanss/rig/internal/project"
)

func TestDatabaseElectricDefaults(t *testing.T) {
	p, out := parseFiles(t, "database:\n  electric:\n    enabled: true\n")
	if out != "" {
		t.Fatalf("a bare enabled block should be valid:\n%s", out)
	}

	e := p.Config.Database.Electric
	if e.Image != project.DefaultElectricImage {
		t.Errorf("image = %q, want %q", e.Image, project.DefaultElectricImage)
	}
	if e.ContainerName != "demo-electric" {
		t.Errorf("container_name = %q, want demo-electric", e.ContainerName)
	}
	if e.Port != project.DefaultElectricPort {
		t.Errorf("port = %d, want %d", e.Port, project.DefaultElectricPort)
	}

	// The sync service follows the database over logical replication, which
	// cannot be turned on after the server has started — so enabling it adds
	// the setting when nothing else names one.
	if !slices.Contains(p.Config.Database.Settings, "wal_level=logical") {
		t.Errorf("settings = %v, want wal_level=logical added", p.Config.Database.Settings)
	}
}

func TestDatabaseSettingsAreKeptAsWritten(t *testing.T) {
	p, out := parseFiles(t,
		"database:\n  settings:\n    - max_connections=200\n    - wal_level=replica\n  electric:\n    enabled: true\n")
	if out != "" {
		t.Fatalf("this configuration should be accepted:\n%s", out)
	}

	// A project that pins its own wal_level wrote it down for a reason, so
	// electric.enabled must not quietly override it.
	got := p.Config.Database.Settings
	want := []string{"max_connections=200", "wal_level=replica"}
	if !slices.Equal(got, want) {
		t.Errorf("settings = %v, want %v", got, want)
	}
}

func TestDatabaseElectricOffAddsNothing(t *testing.T) {
	p, out := parseFiles(t, "database:\n  port: 55440\n")
	if out != "" {
		t.Fatalf("this configuration should be accepted:\n%s", out)
	}
	if len(p.Config.Database.Settings) != 0 {
		t.Errorf("settings = %v, want none", p.Config.Database.Settings)
	}
	if e := p.Config.Database.Electric; e.Enabled || e.Image != "" || e.Port != 0 {
		t.Errorf("electric should stay zero when not enabled: %+v", e)
	}
}
