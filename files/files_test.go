package files

import (
	"io"
	"strings"
	"testing"
)

// The difference between refusing and truncating is silent data loss.
//
// io.LimitReader reports EOF at the limit, which to a store looks like a
// complete object one byte short of the truth. A file exactly at the cap has to
// be accepted, and the byte after it is what proves there was one.
func TestTheCapRefusesRatherThanTruncating(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		size    int
		cap     int64
		wantErr bool
	}{
		{"under", 9, 10, false},
		{"exactly at the cap", 10, 10, false},
		{"one byte over", 11, 10, true},
		{"far over", 1000, 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &limitedReader{r: strings.NewReader(strings.Repeat("x", tc.size)), left: tc.cap}

			got, err := io.ReadAll(l)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("read %d bytes with a cap of %d and no error", len(got), tc.cap)
			case tc.wantErr && !l.exceeded:
				t.Error("the reader failed without saying the cap was the reason")
			case !tc.wantErr && err != nil:
				t.Fatalf("err = %v", err)
			case !tc.wantErr && len(got) != tc.size:
				t.Errorf("read %d bytes, want %d", len(got), tc.size)
			}
		})
	}
}

// The type is decided from the bytes, and the reader still has all of them
// afterwards: the body is a network stream and there is no second pass.
func TestSniffingDecidesTheTypeAndKeepsTheBytes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, body, want string }{
		{"png", "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 20), "image/png"},
		{"html", "<!DOCTYPE html><html></html>", "text/html; charset=utf-8"},
		{"empty", "", "text/plain; charset=utf-8"},
		{"longer than the sniff window", strings.Repeat("A", 2000), "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, got := sniff(strings.NewReader(tc.body))
			if got != tc.want {
				t.Errorf("type = %q, want %q", got, tc.want)
			}

			all, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(all) != tc.body {
				t.Errorf("sniffing left %d bytes of the original %d", len(all), len(tc.body))
			}
		})
	}
}

// The name goes on the row and into a header and a URL segment. It never
// becomes the storage key — see blob.Key — so this is not the last line of
// defence, and it should not have to be.
func TestANameIsStrippedBackToSomethingSafe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"cover.png", "cover.png"},
		{"../../etc/passwd", "passwd"},
		{`C:\Windows\evil.exe`, "evil.exe"},
		{"with\"quote.txt", "withquote.txt"},
		{"line\r\nbreak.txt", "linebreak.txt"},
		{"räksmörgås.pdf", "räksmörgås.pdf"},
		{"", "file"},
		{"///", "file"},
	} {
		if got := cleanName(tc.in); got != tc.want {
			t.Errorf("cleanName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A statement is built around a table and a column name, and both come from the
// document rather than from a request. The check is here anyway, because "no
// request reaches this" is a property of every caller rather than of the
// function.
func TestAnIdentifierThatCouldNotHaveBeenGeneratedIsRefused(t *testing.T) {
	t.Parallel()

	if got := quoteIdent("cover_file_id"); got != `"cover_file_id"` {
		t.Errorf("quoteIdent = %q", got)
	}

	defer func() {
		if recover() == nil {
			t.Error("an identifier with a quote in it should not reach a statement")
		}
	}()
	quoteIdent(`id"; DROP TABLE rig_file; --`)
}
