package cli_test

import (
	"strings"
	"testing"
)

// A test binary has no module version and no stamp, so what `rig version`
// prints here is the development answer. That is the case worth pinning: the
// command has to say something rather than nothing when it is asked by
// somebody who built rig themselves.
func TestVersion(t *testing.T) {
	out, _, code := run(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("version printed nothing")
	}
}

func TestVersionVerbose(t *testing.T) {
	out, _, code := run(t, "version", "--verbose")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "rig ") {
		t.Errorf("verbose output does not lead with the version:\n%s", out)
	}
	// The toolchain is in the build info of every binary, test binaries
	// included, so its absence means the build info was not read at all.
	if !strings.Contains(out, "go       go") {
		t.Errorf("verbose output does not name the toolchain:\n%s", out)
	}
}
