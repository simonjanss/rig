package observe

import "testing"

// The span name is the verb and the table, because a generated INSERT names
// every column it writes and a trace listing that as a name is unreadable at
// the width anybody views one.
func TestQueryName(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"SELECT id, title FROM todo WHERE tenant_id = $1", "SELECT todo"},
		{"INSERT INTO todo (id, title) VALUES ($1, $2) RETURNING id", "INSERT todo"},
		{"UPDATE todo SET title = $1 WHERE id = $2", "UPDATE todo"},
		{"DELETE FROM todo WHERE id = $1", "DELETE todo"},
		{"\n\tSELECT count(*)\n\tFROM todo_attachment\n", "SELECT todo_attachment"},

		// Nothing this looks for, so the verb alone. Being right about the
		// ordinary statement and quiet about the unusual one beats being
		// confidently wrong about both.
		{"WITH ranked AS (SELECT 1) SELECT * FROM ranked", "WITH"},
		{"SELECT 1", "SELECT"},
		{"BEGIN", "BEGIN"},
		{"", "query"},
	} {
		if got := queryName(tc.sql); got != tc.want {
			t.Errorf("queryName(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}
