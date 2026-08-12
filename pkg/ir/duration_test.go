package ir_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/simonjanss/rig/pkg/ir"
)

// The point of the type is that a document is read by more than Go, so a
// duration has to survive the round trip through a string without changing what
// it means.
func TestDurationRoundTrips(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		text string
		want time.Duration
		// canonical is the form the document is written with, when it differs
		// from what somebody typed.
		canonical string
	}{
		{text: "0", want: 0, canonical: "0s"},
		{text: "0s", want: 0},
		{text: "250ms", want: 250 * time.Millisecond},
		{text: "45s", want: 45 * time.Second},
		{text: "15m", want: 15 * time.Minute},
		{text: "12h", want: 12 * time.Hour},
		// The extension, and the reason this is not time.ParseDuration: a month of
		// "remember me" is written 30d by everybody who writes it down.
		{text: "30d", want: 30 * 24 * time.Hour},
		{text: "1d12h", want: 36 * time.Hour, canonical: "1d12h"},
		{text: "90m", want: 90 * time.Minute, canonical: "1h30m"},
		{text: "720h", want: 30 * 24 * time.Hour, canonical: "30d"},
		{text: "0.5d", want: 12 * time.Hour, canonical: "12h"},
	} {
		t.Run(c.text, func(t *testing.T) {
			t.Parallel()

			got, err := ir.ParseDuration(c.text)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", c.text, err)
			}
			if got != c.want {
				t.Fatalf("ParseDuration(%q) = %s, want %s", c.text, got, c.want)
			}

			want := c.canonical
			if want == "" {
				want = c.text
			}
			if formatted := ir.FormatDuration(got); formatted != want {
				t.Errorf("FormatDuration(%s) = %q, want %q", got, formatted, want)
			}

			// And the whole way out and back, which is what a generator on the
			// other side of the document does.
			encoded, err := json.Marshal(ir.Duration(got))
			if err != nil {
				t.Fatal(err)
			}
			var back ir.Duration
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("unmarshal %s: %v", encoded, err)
			}
			if back.Duration() != c.want {
				t.Errorf("%s survived as %s", encoded, back)
			}
		})
	}
}

func TestDurationRejectsWhatIsNotOne(t *testing.T) {
	t.Parallel()

	for _, text := range []string{"", "soon", "10", "m10", "10 minutes", "ten"} {
		if got, err := ir.ParseDuration(text); err == nil {
			t.Errorf("ParseDuration(%q) = %s, want an error", text, got)
		}
	}
}

// A bare zero is accepted because that is how "off" reads in a configuration
// file, and cache_ttl is the field that needs it.
func TestDurationUnmarshalsABareZero(t *testing.T) {
	t.Parallel()

	var d ir.Duration
	if err := json.Unmarshal([]byte("0"), &d); err != nil {
		t.Fatalf("a bare 0 should decode: %v", err)
	}
	if d != 0 {
		t.Errorf("d = %s, want 0", d)
	}

	if err := json.Unmarshal([]byte("600"), &d); err == nil {
		t.Error("a bare number that is not zero has no unit and should be refused")
	}
}
