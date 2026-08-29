package diag_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/diag"
)

func TestCodesAreUniqueAndDocumented(t *testing.T) {
	t.Parallel()

	// Duplicate IDs panic at registration, so reaching here already proves
	// uniqueness. What is left to check is that each code is usable: it needs
	// an ID, a default severity, and a summary for the generated docs.
	codes := diag.Codes()
	if len(codes) == 0 {
		t.Fatal("no codes registered")
	}

	for _, c := range codes {
		if !strings.HasPrefix(c.ID, "RIG") || len(c.ID) != 7 {
			t.Errorf("code %q should look like RIGnnnn", c.ID)
		}
		if c.Severity == "" {
			t.Errorf("code %s has no default severity", c.ID)
		}
		if c.Summary == "" {
			t.Errorf("code %s has no summary", c.ID)
		}
		if !strings.HasSuffix(c.Summary, ".") {
			t.Errorf("code %s summary should be a sentence: %q", c.ID, c.Summary)
		}
	}
}

func TestCodesAreSortedByID(t *testing.T) {
	t.Parallel()

	codes := diag.Codes()
	for i := 1; i < len(codes); i++ {
		if codes[i-1].ID >= codes[i].ID {
			t.Fatalf("Codes() is not sorted: %s came before %s", codes[i-1].ID, codes[i].ID)
		}
	}
}

func TestLookupCode(t *testing.T) {
	t.Parallel()

	got, ok := diag.LookupCode(diag.CodeUnknownColumn.ID)
	if !ok || got.ID != diag.CodeUnknownColumn.ID {
		t.Fatalf("LookupCode(%s) = %v, %v", diag.CodeUnknownColumn.ID, got, ok)
	}
	if _, ok := diag.LookupCode("RIG0000"); ok {
		t.Fatal("LookupCode of an unregistered id should report false")
	}
}

func TestAddUsesTheCodeSeverityAndHint(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 3, Column: 5}, "column %q is gone", "title")

	all := l.All()
	if len(all) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(all))
	}
	d := all[0]
	if d.Severity != diag.SeverityError {
		t.Errorf("severity = %q, want error", d.Severity)
	}
	if d.Message != `column "title" is gone` {
		t.Errorf("message = %q", d.Message)
	}
	if d.Hint != diag.CodeUnknownColumn.Hint {
		t.Errorf("hint was not carried from the code")
	}
	if got := d.Anchor.String(); got != "a.yaml:3:5" {
		t.Errorf("anchor = %q, want a.yaml:3:5", got)
	}
}

func TestAddSeverityOverridesAndEmptyDrops(t *testing.T) {
	t.Parallel()

	var l diag.List
	// A convention rule configured as a warning reports as a warning even
	// though its code defaults to error.
	l.AddSeverity(diag.CodeMissingColumnComment, diag.SeverityWarning, diag.Anchor{}, "no comment")
	// A rule configured off reports nothing at all.
	l.AddSeverity(diag.CodeMissingColumnComment, "", diag.Anchor{}, "no comment")

	if l.Len() != 1 {
		t.Fatalf("got %d diagnostics, want 1 (the disabled rule should be dropped)", l.Len())
	}
	if l.All()[0].Severity != diag.SeverityWarning {
		t.Errorf("severity override did not take effect")
	}
	if l.HasErrors() {
		t.Errorf("a warning should not count as an error")
	}
}

func TestNilListDiscards(t *testing.T) {
	t.Parallel()

	// A stage called without a collector must not panic.
	var l *diag.List
	l.Add(diag.CodeInternal, diag.Anchor{}, "ignored")
	l.AddSeverity(diag.CodeInternal, diag.SeverityError, diag.Anchor{}, "ignored")
	l.Append(diag.List{})

	if l.Len() != 0 || l.HasErrors() || l.Err() != nil || l.All() != nil {
		t.Fatal("a nil list should stay empty and report nothing")
	}
}

func TestOrderIsFileThenPositionThenCode(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "b.yaml", Line: 1}, "b1")
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 9}, "a9")
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 2, Column: 7}, "a2c7")
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 2, Column: 1}, "a2c1")
	l.Add(diag.CodeInternal, diag.Anchor{}, "global")

	var got []string
	for _, d := range l.All() {
		got = append(got, d.Message)
	}

	want := []string{"global", "a2c1", "a2c7", "a9", "b1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSameLineOrdersByCode(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownTable, diag.Anchor{File: "a.yaml", Line: 1}, "second")
	l.Add(diag.CodeUnmentionedColumn, diag.Anchor{File: "a.yaml", Line: 1}, "first")

	all := l.All()
	if all[0].Code.ID != diag.CodeUnmentionedColumn.ID {
		t.Fatalf("expected RIG3100 before RIG3102, got %s then %s", all[0].Code.ID, all[1].Code.ID)
	}
}

func TestErrReportsEveryErrorTogether(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 1}, "first problem")
	l.AddSeverity(diag.CodeMissingColumnComment, diag.SeverityWarning, diag.Anchor{File: "a.yaml", Line: 2}, "just a warning")
	l.Add(diag.CodeUnknownTable, diag.Anchor{File: "b.yaml", Line: 3}, "second problem")

	err := l.Err()
	if err == nil {
		t.Fatal("Err() should be non-nil when there are errors")
	}
	msg := err.Error()
	for _, want := range []string{"2 validation errors", "first problem", "second problem"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "just a warning") {
		t.Errorf("warnings should not appear in the error:\n%s", msg)
	}
}

func TestErrIsNilWithoutErrors(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.AddSeverity(diag.CodeBooleanPrefix, diag.SeverityWarning, diag.Anchor{}, "w")
	l.AddSeverity(diag.CodeEndpointShadowed, diag.SeverityInfo, diag.Anchor{}, "i")

	if l.Err() != nil {
		t.Fatalf("Err() should be nil with only warnings and notes, got %v", l.Err())
	}
}

func TestRenderText(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "services/lesson/lesson.yaml", Line: 12, Column: 5}, "column %q does not exist", "titel")
	l.AddSeverity(diag.CodeBooleanPrefix, diag.SeverityWarning, diag.Anchor{File: "services/lesson/lesson.yaml", Line: 20}, "rename to is_active")

	var b strings.Builder
	if err := diag.Render(&b, &l, diag.FormatText, diag.RenderOptions{Hints: true}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"services/lesson/lesson.yaml",
		"12:5: error[RIG3101]: column \"titel\" does not exist",
		"20: warning[RIG6020]: rename to is_active",
		"1 error, 1 warning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output is missing %q:\n%s", want, out)
		}
	}

	// The file heading appears once, not per diagnostic.
	if n := strings.Count(out, "services/lesson/lesson.yaml"); n != 1 {
		t.Errorf("file heading appeared %d times, want 1:\n%s", n, out)
	}
}

func TestRenderTextIsSilentWhenClean(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	var l diag.List
	if err := diag.Render(&b, &l, diag.FormatText, diag.RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if b.Len() != 0 {
		t.Fatalf("a clean run should print nothing, got %q", b.String())
	}
}

func TestRenderGitHubEscapesNewlines(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 4, Column: 2}, "line one\nline two")

	var b strings.Builder
	if err := diag.Render(&b, &l, diag.FormatGitHub, diag.RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := strings.TrimRight(b.String(), "\n")

	if strings.Contains(out, "\n") {
		t.Fatalf("a workflow command must stay on one line:\n%q", out)
	}
	want := "::error file=a.yaml,line=4,col=2,title=RIG3101::line one%0Aline two"
	if out != want {
		t.Fatalf("github output =\n%q\nwant\n%q", out, want)
	}
}

func TestRenderJSON(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 1}, "boom")
	l.AddSeverity(diag.CodeBooleanPrefix, diag.SeverityWarning, diag.Anchor{}, "meh")

	var b strings.Builder
	if err := diag.Render(&b, &l, diag.FormatJSON, diag.RenderOptions{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()

	for _, want := range []string{`"errors": 1`, `"warnings": 1`, `"RIG3101"`, `"boom"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output is missing %q:\n%s", want, out)
		}
	}
	// An anchorless diagnostic should omit the key rather than emit a zero object.
	if strings.Contains(out, `"anchor": {}`) {
		t.Errorf("empty anchors should be omitted:\n%s", out)
	}
}

func TestParseFormat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in      string
		want    diag.Format
		wantErr bool
	}{
		{"", diag.FormatText, false},
		{"text", diag.FormatText, false},
		{"JSON", diag.FormatJSON, false},
		{" github ", diag.FormatGitHub, false},
		{"xml", "", true},
	} {
		got, err := diag.ParseFormat(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseFormat(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want diag.Severity
		ok   bool
	}{
		{"error", diag.SeverityError, true},
		{"warn", diag.SeverityWarning, true},
		{"warning", diag.SeverityWarning, true},
		{"info", diag.SeverityInfo, true},
		{"off", "", false},
		{"", "", false},
	} {
		got, ok := diag.ParseSeverity(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseSeverity(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAnchorString(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		anchor diag.Anchor
		want   string
	}{
		{diag.Anchor{File: "a.yaml", Line: 2, Column: 3}, "a.yaml:2:3"},
		{diag.Anchor{File: "a.yaml", Line: 2}, "a.yaml:2"},
		{diag.Anchor{File: "a.yaml"}, "a.yaml"},
		{diag.At("tables.lesson.columns.title"), "tables.lesson.columns.title"},
		{diag.Anchor{}, ""},
	} {
		if got := tc.anchor.String(); got != tc.want {
			t.Errorf("Anchor%+v.String() = %q, want %q", tc.anchor, got, tc.want)
		}
	}
}

// Color belongs to a terminal, and the escape codes have to be the ones that
// actually reset — a stray unreset color bleeds into everything the shell
// prints afterwards.
func TestColorIsPerSeverityAndAlwaysReset(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 1, Column: 1}, "an error")
	l.Add(diag.CodeUnmentionedColumn, diag.Anchor{File: "a.yaml", Line: 2, Column: 1}, "a warning")

	var colored strings.Builder
	if err := diag.Render(&colored, &l, diag.FormatText, diag.RenderOptions{Color: true}); err != nil {
		t.Fatal(err)
	}
	out := colored.String()

	if !strings.Contains(out, "\033[31m") || !strings.Contains(out, "\033[33m") {
		t.Errorf("an error should be red and a warning yellow:\n%q", out)
	}
	if strings.Count(out, "\033[0m") < 2 {
		t.Errorf("every color should be reset:\n%q", out)
	}

	// And a pipe gets none of it.
	var plain strings.Builder
	if err := diag.Render(&plain, &l, diag.FormatText, diag.RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\033[") {
		t.Errorf("uncolored output should have no escapes:\n%q", plain.String())
	}
}

// String is what an error message and a test failure both use, so it has to be
// the plain rendering rather than whatever the terminal would have got.
func TestListStringIsThePlainRendering(t *testing.T) {
	t.Parallel()

	var l diag.List
	l.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 3, Column: 5}, "something is wrong")

	out := l.String()
	if strings.Contains(out, "\033[") {
		t.Errorf("String should not be colored:\n%q", out)
	}
	if !strings.Contains(out, "3:5") || !strings.Contains(out, "something is wrong") {
		t.Errorf("String should carry the position and the message:\n%s", out)
	}
	// The hint is included: String is what a caller prints when it has nowhere
	// else to put it.
	if diag.CodeUnknownColumn.Hint != "" && !strings.Contains(out, diag.CodeUnknownColumn.Hint) {
		t.Errorf("String should carry the hint:\n%s", out)
	}

	if empty := (&diag.List{}).String(); empty != "" {
		t.Errorf("an empty list renders to %q, want nothing", empty)
	}
}

// Append is how a stage's diagnostics reach the caller's list, and it is called
// once per stage whether or not the stage found anything.
func TestAppendMergesAndToleratesNothing(t *testing.T) {
	t.Parallel()

	var into diag.List
	into.Add(diag.CodeUnknownColumn, diag.Anchor{File: "a.yaml", Line: 1}, "first")

	into.Append(diag.List{})
	if len(into.All()) != 1 {
		t.Errorf("appending nothing changed the list: %v", into.All())
	}

	var other diag.List
	other.Add(diag.CodeUnmentionedColumn, diag.Anchor{File: "b.yaml", Line: 1}, "second")
	into.Append(other)

	if len(into.All()) != 2 {
		t.Errorf("got %d diagnostics, want both", len(into.All()))
	}

	// A nil list discards, so a stage can be called without a collector.
	var none *diag.List
	none.Append(other)
	none.Add(diag.CodeUnknownColumn, diag.Anchor{}, "dropped")
	if none.HasErrors() {
		t.Error("a nil list holds nothing")
	}
}

// A diagnostic with nowhere to point still has to read as one: some describe
// the project rather than a line in a file.
func TestADiagnosticWithNoPositionStillReads(t *testing.T) {
	t.Parallel()

	d := diag.Diagnostic{Code: diag.CodeUnknownColumn, Severity: diag.SeverityError, Message: "no generators are configured"}

	got := d.String()
	if !strings.HasPrefix(got, string(diag.SeverityError)) {
		t.Errorf("String = %q, want it to start with the severity", got)
	}
	if !strings.Contains(got, diag.CodeUnknownColumn.ID) || !strings.Contains(got, "no generators") {
		t.Errorf("String = %q", got)
	}
}
