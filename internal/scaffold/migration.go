package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/simonjanss/rig/internal/migcheck"
)

// MigrationOptions describe a new migration.
type MigrationOptions struct {
	// Name is the descriptive half of the filename.
	Name string
	// Table, when set, scaffolds a CREATE TABLE with the conventional columns.
	Table string
	// SoftDelete adds the columns that make the table soft-deletable.
	SoftDelete bool
	// Snapshot adds the snapshot triple and its CHECK constraint.
	Snapshot bool
}

// NextMigrationNumber reads the directory and returns the number after the
// highest one present.
//
// Numbering by sequence rather than by timestamp keeps the directory readable
// and the order obvious. It does mean two people adding a migration on the same
// day will collide — which is the point: a merge conflict on the filename is a
// far better outcome than two migrations silently interleaving.
//
// The highest is read with [migcheck.Version] rather than by rig's own naming
// rule, so a file rig would not have written still counts. 25_by_hand.sql is
// version 25 to goose whatever rig thinks of the name, and numbering the next
// migration 1 because the rule did not recognize it would produce the one thing
// this function exists to avoid.
func NextMigrationNumber(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", dir, err)
	}

	var highest int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if n, ok := migcheck.Version(entry.Name()); ok {
			highest = max(highest, n)
		}
	}
	return int(highest) + 1, nil
}

// MigrationFilename builds the path for a new migration.
func MigrationFilename(dir string, number int, name string) string {
	return filepath.Join(dir, fmt.Sprintf("%05d_%s.sql", number, snake(name)))
}

// Migration renders a migration file.
func Migration(opt MigrationOptions) string {
	var b strings.Builder

	b.WriteString("-- +goose Up\n-- +goose StatementBegin\n\n")
	if opt.Table != "" {
		b.WriteString(createTable(opt))
	} else {
		b.WriteString("-- Write the migration here.\n")
	}
	b.WriteString("\n-- +goose StatementEnd\n\n")

	b.WriteString("-- +goose Down\n-- +goose StatementBegin\n\n")
	if opt.Table != "" {
		fmt.Fprintf(&b, "DROP TABLE %s;\n", opt.Table)
		if opt.Snapshot {
			fmt.Fprintf(&b, "DROP TYPE %s_version_type;\n", opt.Table)
		}
	} else {
		b.WriteString("-- Undo the migration here.\n")
	}
	b.WriteString("\n-- +goose StatementEnd\n")

	return b.String()
}

func createTable(opt MigrationOptions) string {
	t := opt.Table

	// Elements are collected rather than written straight out, so that the last
	// one can be emitted without a trailing comma. A scaffold that does not
	// apply as written is worse than none: it fails on the first `rig db up`
	// with a syntax error nobody expected to be theirs.
	elements := []string{
		"id                      uuid PRIMARY KEY",
		"tenant_id               uuid NOT NULL",
	}

	// Your columns go between identity and bookkeeping, which is where a
	// reader looks for them.
	const marker = "-- Add your columns here."
	markerAfter := len(elements)

	elements = append(elements,
		"created_at              timestamptz NOT NULL DEFAULT now()",
		"created_by_account_id   uuid",
		"updated_at              timestamptz",
		"updated_by_account_id   uuid",
	)

	if opt.SoftDelete {
		elements = append(elements,
			"deleted_at              timestamptz",
			"deleted_by_account_id   uuid",
		)
	}

	if opt.Snapshot {
		elements = append(elements,
			fmt.Sprintf("version_type            %s_version_type NOT NULL DEFAULT 'Original'", t),
			fmt.Sprintf("snapshot_from_%s_id%s uuid REFERENCES %s(id)", t, pad(t), t),
			fmt.Sprintf("snapshot_from_%s_at%s timestamptz", t, pad(t)),
		)

		// The constraint is what keeps a snapshot immutable and an original
		// unmarked. Without it the two states are only a convention, and one
		// stray UPDATE turns history into fiction.
		elements = append(elements, fmt.Sprintf(`CONSTRAINT %s_version_check CHECK (
        (version_type = 'Original'
            AND snapshot_from_%s_id IS NULL
            AND snapshot_from_%s_at IS NULL)
        OR
        (version_type = 'Snapshot'
            AND snapshot_from_%s_id IS NOT NULL
            AND snapshot_from_%s_at IS NOT NULL
            AND updated_at IS NULL%s)
    )`, t, t, t, t, t, snapshotDeleteClause(opt)))
	}

	var b strings.Builder

	if opt.Snapshot {
		fmt.Fprintf(&b, "CREATE TYPE %s_version_type AS ENUM ('Original', 'Snapshot');\n\n", t)
	}

	fmt.Fprintf(&b, "CREATE TABLE %s (\n", t)
	for i, el := range elements {
		if i == markerAfter {
			fmt.Fprintf(&b, "\n    %s\n\n", marker)
		}
		comma := ","
		if i == len(elements)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %s%s\n", el, comma)
	}
	b.WriteString(");\n\n")

	fmt.Fprintf(&b, "COMMENT ON TABLE %s IS 'TODO: what is this table for?';\n\n", t)

	// Every generated query filters by tenant, so this index is not an
	// optimization — it is the difference between a seek and a full scan.
	fmt.Fprintf(&b, "CREATE INDEX %s_tenant_created_idx ON %s (tenant_id, created_at DESC);\n", t, t)
	if opt.Snapshot {
		// Deliberately not partial. A partial index would be smaller, but the
		// foreign-key check Postgres runs when a row is deleted looks across
		// every row, so it could not use one.
		fmt.Fprintf(&b, "CREATE INDEX %s_snapshot_idx ON %s (snapshot_from_%s_id);\n", t, t, t)
	}

	return b.String()
}

func snapshotDeleteClause(opt MigrationOptions) string {
	if !opt.SoftDelete {
		return ""
	}
	return "\n            AND deleted_at IS NULL"
}

// pad keeps the scaffolded column names aligned regardless of table-name length.
func pad(table string) string {
	return strings.Repeat(" ", max(24-len("snapshot_from_"+table+"_id")-1, 1))
}

// snake normalizes a migration name into the form the filename rule requires.
func snake(name string) string {
	var b strings.Builder
	prevUnderscore := false

	for _, r := range strings.TrimSpace(strings.ToLower(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}

	return strings.Trim(b.String(), "_")
}
