// Package version answers what build of rig this is.
//
// Three kinds of binary have to answer it, and only one of them is built by
// the release:
//
//   - `go install github.com/simonjanss/rig/cmd/rig@v0.3.0` stamps nothing, but
//     the module version is in the build info, and it is exact.
//   - A release archive is built with -ldflags, because goreleaser knows the
//     tag before the compiler does.
//   - `make build` from a checkout has neither, and what is useful there is the
//     commit — which the toolchain records on its own, except in a git worktree,
//     where it records nothing. So the commit is reported when it is there and
//     the version stands alone when it is not, rather than being relied on.
//
// So the answer is assembled from whatever of those is present, in that order
// of trust, and never guessed.
package version

import (
	"runtime/debug"
	"strings"
)

// stamped is set at link time by the release build:
//
//	-ldflags "-X github.com/simonjanss/rig/internal/version.stamped=v0.3.0"
//
// It is empty in every other build.
var stamped string

// Info is what a build knows about itself.
type Info struct {
	// Version is a semantic version for a released build, "(devel)" for one
	// built from a checkout, and "unknown" when there is no build info at all —
	// which happens under `go test`, and is not worth pretending otherwise.
	Version string

	// Revision is the commit, when the toolchain recorded one. Empty for a
	// build made from an unpacked module zip, which carries no repository.
	Revision string

	// Time is the commit time, in RFC 3339, on the same terms as Revision.
	Time string

	// Modified reports that the working tree had uncommitted changes. A true
	// here is why two binaries reporting the same Revision can differ.
	Modified bool

	// Go is the toolchain that built it.
	Go string
}

// Get reads this binary's own build information.
func Get() Info {
	info := Info{Version: "unknown"}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if stamped != "" {
			info.Version = stamped
		}
		return info
	}

	info.Go = bi.GoVersion
	if bi.Main.Version != "" {
		info.Version = bi.Main.Version
	}
	// The stamp wins over the build info: a release archive is built from a
	// checkout, so its Main.Version is "(devel)" and the tag is the only place
	// the real answer exists.
	if stamped != "" {
		info.Version = stamped
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.time":
			info.Time = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// String is the one-line version, which is what `rig --version` prints and what
// travels into the compiled document as the tool that produced it.
//
// A development build says so and names the commit, because "dev" alone in a
// generated file's provenance answers nothing.
func (i Info) String() string {
	if i.Version != "(devel)" && i.Version != "unknown" {
		return i.Version
	}

	var b strings.Builder
	b.WriteString("(devel)")
	if i.Revision != "" {
		b.WriteString(" ")
		b.WriteString(short(i.Revision))
		if i.Modified {
			b.WriteString("-dirty")
		}
	}
	return b.String()
}

// short is a commit abbreviated the way git abbreviates it.
func short(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// String is the version of the running binary.
func String() string { return Get().String() }
