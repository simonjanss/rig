// Package naming converts between the three casings rig deals with: the
// snake_case of Postgres, the PascalCase of Go and TypeScript identifiers, and
// the camelCase of JSON keys.
//
// The conversions are deliberately asymmetric. A Go identifier uppercases known
// initialisms, so fixture_id becomes FixtureID; a JSON key does not, so the same
// column becomes fixtureId. Applying Go's initialism convention to wire formats
// produces keys like fixtureID that no other language's client would generate.
package naming

import (
	"maps"
	"strings"
	"unicode"
)

// DefaultInitialisms are the acronyms that stay fully uppercase in Go
// identifiers. A project adds its own through [Config].
var DefaultInitialisms = []string{
	"ACL", "API", "ASCII", "CIDR", "CPU", "CSS", "CSV", "DB", "DNS", "EOF",
	"GUID", "HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "JWT", "LHS", "MIME",
	"QPS", "RAM", "RHS", "RPC", "SLA", "SMTP", "SQL", "SSH", "SSL", "TCP",
	"TLS", "TTL", "UDP", "UI", "UID", "URI", "URL", "UTF8", "UUID", "VM",
	"XML", "XSS", "YAML",
}

// Case is the shape of a JSON key.
type Case string

const (
	CaseCamel  Case = "camel"
	CasePascal Case = "pascal"
	CaseSnake  Case = "snake"
)

// Config configures a [Namer].
type Config struct {
	// Initialisms replaces [DefaultInitialisms] when non-empty.
	Initialisms []string
	// ExtraInitialisms are added to whichever base list is in use. This is the
	// usual knob: a project adds its own acronyms without restating Go's.
	ExtraInitialisms []string
	// Plurals overrides the derived plural for specific singular names, keyed
	// by the singular form as written in the database.
	Plurals map[string]string
	// JSONCase is the shape of generated JSON keys. Defaults to camel.
	JSONCase Case
}

// Namer performs the conversions. The zero value is not usable; call [New].
type Namer struct {
	initialisms map[string]string // upper-cased word -> canonical form
	plurals     map[string]string
	jsonCase    Case
}

// New builds a namer from a configuration.
func New(cfg Config) *Namer {
	base := cfg.Initialisms
	if len(base) == 0 {
		base = DefaultInitialisms
	}

	init := make(map[string]string, len(base)+len(cfg.ExtraInitialisms))
	for _, w := range append(append([]string{}, base...), cfg.ExtraInitialisms...) {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		init[strings.ToUpper(w)] = strings.ToUpper(w)
	}

	plurals := make(map[string]string, len(cfg.Plurals))
	maps.Copy(plurals, cfg.Plurals)

	jc := cfg.JSONCase
	if jc == "" {
		jc = CaseCamel
	}

	return &Namer{initialisms: init, plurals: plurals, jsonCase: jc}
}

// Words splits an identifier into its component words, accepting snake_case,
// kebab-case, PascalCase, camelCase, or any mixture. Runs of capitals are kept
// together up to the last one that starts a new word, so "APIKey" splits into
// "API" and "Key" rather than into single letters.
func Words(s string) []string {
	var (
		words []string
		cur   []rune
	)
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}

	runes := []rune(s)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == ' ' || r == '.':
			flush()
		case unicode.IsUpper(r):
			// A capital after a lowercase or a digit always starts a word.
			if i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
				flush()
			} else if i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]) {
				// Last capital of a run that continues in lowercase: it belongs
				// to the next word, not the acronym. "APIKey" -> "API", "Key".
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()

	return words
}

// Go converts an identifier to an exported Go name, uppercasing initialisms.
//
//	email_address -> EmailAddress
//	fixture_id    -> FixtureID
//	api_key       -> APIKey
func (n *Namer) Go(s string) string {
	var b strings.Builder
	for _, w := range Words(s) {
		b.WriteString(n.word(w))
	}
	return b.String()
}

// GoUnexported is [Namer.Go] with a lowercase first word, for local variables
// and unexported fields. An initialism at the start is lowercased whole, so
// "id_token" becomes "idToken" rather than "iDToken".
func (n *Namer) GoUnexported(s string) string {
	words := Words(s)
	if len(words) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(strings.ToLower(words[0]))
	for _, w := range words[1:] {
		b.WriteString(n.word(w))
	}
	return b.String()
}

// word title-cases a single word, or uppercases it if it is an initialism.
func (n *Namer) word(w string) string {
	if canon, ok := n.initialisms[strings.ToUpper(w)]; ok {
		return canon
	}
	return title(w)
}

func title(w string) string {
	if w == "" {
		return ""
	}
	r := []rune(strings.ToLower(w))
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// JSON converts an identifier to a JSON key in the configured case. Initialisms
// are not uppercased: fixture_id becomes fixtureId, because that is what a
// client generator in any other language would produce.
func (n *Namer) JSON(s string) string {
	words := Words(s)
	if len(words) == 0 {
		return ""
	}

	switch n.jsonCase {
	case CaseSnake:
		lower := make([]string, len(words))
		for i, w := range words {
			lower[i] = strings.ToLower(w)
		}
		return strings.Join(lower, "_")

	case CasePascal:
		var b strings.Builder
		for _, w := range words {
			b.WriteString(title(w))
		}
		return b.String()

	default: // camel
		var b strings.Builder
		b.WriteString(strings.ToLower(words[0]))
		for _, w := range words[1:] {
			b.WriteString(title(w))
		}
		return b.String()
	}
}

// Snake converts an identifier to snake_case.
func Snake(s string) string {
	words := Words(s)
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, "_")
}

// Kebab converts an identifier to kebab-case, used for URL path segments.
func Kebab(s string) string {
	words := Words(s)
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, "-")
}

// Plural returns the plural of a singular name. The override map wins; failing
// that, a handful of English rules apply. The rules are deliberately shallow —
// they get the common cases right and the override map exists for the rest.
func (n *Namer) Plural(s string) string {
	if p, ok := n.plurals[s]; ok {
		return p
	}
	if p, ok := n.plurals[Snake(s)]; ok {
		return p
	}
	return pluralize(s)
}

func pluralize(s string) string {
	if s == "" {
		return ""
	}

	lower := strings.ToLower(s)
	switch {
	case strings.HasSuffix(lower, "s"),
		strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "z"),
		strings.HasSuffix(lower, "ch"),
		strings.HasSuffix(lower, "sh"):
		return s + "es"

	case strings.HasSuffix(lower, "y") && len(s) > 1 && !isVowel(rune(lower[len(lower)-2])):
		return s[:len(s)-1] + "ies"

	default:
		return s + "s"
	}
}

func isVowel(r rune) bool {
	switch r {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	default:
		return false
	}
}

// PathSegment converts a plural resource name into a URL path segment.
//
//	LessonTimes -> lesson-times
func (n *Namer) PathSegment(plural string) string { return Kebab(plural) }
