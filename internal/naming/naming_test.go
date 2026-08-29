package naming_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/naming"
)

func TestWords(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"title", []string{"title"}},
		{"email_address", []string{"email", "address"}},
		{"EmailAddress", []string{"Email", "Address"}},
		{"emailAddress", []string{"email", "Address"}},
		{"fixture_id", []string{"fixture", "id"}},
		{"FixtureID", []string{"Fixture", "ID"}},
		{"APIKey", []string{"API", "Key"}},
		{"HTTPSProxy", []string{"HTTPS", "Proxy"}},
		{"ID", []string{"ID"}},
		{"lesson-time", []string{"lesson", "time"}},
		{"snapshot_from_lesson_at", []string{"snapshot", "from", "lesson", "at"}},
		{"utf8_value", []string{"utf8", "value"}},
		{"oauth2Token", []string{"oauth2", "Token"}},
	} {
		got := naming.Words(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("Words(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestGoUppercasesInitialisms(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{})
	for _, tc := range []struct{ in, want string }{
		{"title", "Title"},
		{"email_address", "EmailAddress"},
		{"id", "ID"},
		{"fixture_id", "FixtureID"},
		{"api_key", "APIKey"},
		{"tenant_id", "TenantID"},
		{"created_by_account_id", "CreatedByAccountID"},
		{"snapshot_from_lesson_at", "SnapshotFromLessonAt"},
		{"json_payload", "JSONPayload"},
		{"url", "URL"},
		{"EmailAddress", "EmailAddress"},
	} {
		if got := n.Go(tc.in); got != tc.want {
			t.Errorf("Go(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGoWithExtraInitialisms(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{ExtraInitialisms: []string{"scb"}})
	if got := n.Go("scb_code"); got != "SCBCode" {
		t.Errorf("Go(scb_code) = %q, want SCBCode", got)
	}
	// The defaults are still in effect alongside the addition.
	if got := n.Go("api_key"); got != "APIKey" {
		t.Errorf("Go(api_key) = %q, want APIKey", got)
	}
}

func TestGoUnexported(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{})
	for _, tc := range []struct{ in, want string }{
		{"email_address", "emailAddress"},
		{"id_token", "idToken"}, // a leading initialism lowercases whole
		{"fixture_id", "fixtureID"},
		{"title", "title"},
		{"", ""},
	} {
		if got := n.GoUnexported(tc.in); got != tc.want {
			t.Errorf("GoUnexported(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJSONDoesNotUppercaseInitialisms(t *testing.T) {
	t.Parallel()

	// This is the whole point of separating Go names from wire names: a JSON
	// key of "fixtureID" is a Go convention leaking into a language-neutral
	// format, and no other client generator would produce it.
	n := naming.New(naming.Config{})
	for _, tc := range []struct{ in, want string }{
		{"fixture_id", "fixtureId"},
		{"FixtureID", "fixtureId"},
		{"id", "id"},
		{"api_key", "apiKey"},
		{"TeacherEmailAddress", "teacherEmailAddress"},
		{"created_by_account_id", "createdByAccountId"},
		{"", ""},
	} {
		if got := n.JSON(tc.in); got != tc.want {
			t.Errorf("JSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJSONCases(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		jsonCase naming.Case
		want     string
	}{
		{naming.CaseCamel, "teacherEmailAddress"},
		{naming.CasePascal, "TeacherEmailAddress"},
		{naming.CaseSnake, "teacher_email_address"},
		{"", "teacherEmailAddress"}, // camel is the default
	} {
		n := naming.New(naming.Config{JSONCase: tc.jsonCase})
		if got := n.JSON("TeacherEmailAddress"); got != tc.want {
			t.Errorf("JSONCase %q: got %q, want %q", tc.jsonCase, got, tc.want)
		}
	}
}

func TestSnakeAndKebab(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, snake, kebab string }{
		{"LessonTimes", "lesson_times", "lesson-times"},
		{"lesson_time", "lesson_time", "lesson-time"},
		{"FixtureID", "fixture_id", "fixture-id"},
		{"", "", ""},
	} {
		if got := naming.Snake(tc.in); got != tc.snake {
			t.Errorf("Snake(%q) = %q, want %q", tc.in, got, tc.snake)
		}
		if got := naming.Kebab(tc.in); got != tc.kebab {
			t.Errorf("Kebab(%q) = %q, want %q", tc.in, got, tc.kebab)
		}
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{})
	for _, tc := range []struct{ in, want string }{
		{"lesson", "lessons"},
		{"fixture", "fixtures"},
		{"status", "statuses"},
		{"address", "addresses"},
		{"box", "boxes"},
		{"match", "matches"},
		{"dish", "dishes"},
		{"country", "countries"},
		{"day", "days"}, // vowel before y takes a plain s
		{"key", "keys"},
		{"Lesson", "Lessons"},
		{"", ""},
	} {
		if got := n.Plural(tc.in); got != tc.want {
			t.Errorf("Plural(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPluralOverride(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{Plurals: map[string]string{
		"lesson_time": "LessonTimes",
		"person":      "people",
	}})

	if got := n.Plural("lesson_time"); got != "LessonTimes" {
		t.Errorf("Plural(lesson_time) = %q, want the override LessonTimes", got)
	}
	if got := n.Plural("person"); got != "people" {
		t.Errorf("Plural(person) = %q, want the override people", got)
	}
	// An override keyed by the snake form also matches a Pascal lookup, so the
	// same entry serves both the table name and the resource name.
	if got := n.Plural("LessonTime"); got != "LessonTimes" {
		t.Errorf("Plural(LessonTime) = %q, want the override LessonTimes", got)
	}
}

func TestPathSegment(t *testing.T) {
	t.Parallel()

	n := naming.New(naming.Config{})
	for _, tc := range []struct{ in, want string }{
		{"Lessons", "lessons"},
		{"LessonTimes", "lesson-times"},
		{"APIKeys", "api-keys"},
	} {
		if got := n.PathSegment(tc.in); got != tc.want {
			t.Errorf("PathSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
