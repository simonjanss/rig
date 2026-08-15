package servergo

import (
	"fmt"
	"strings"
	"time"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// goDuration renders a duration as Go source a person can check against the
// configuration file.
//
// 30 * 24 * time.Hour rather than 2592000000000000: somebody reading the
// generated wiring has to be able to see that it says thirty days, because the
// whole point of configuring this in a file is that the number is reviewable.
func goDuration(b *gobuf.Buf, d ir.Duration) string {
	v := d.Duration()
	if v == 0 {
		return "0"
	}

	timePkg := b.Import("time")

	var terms []string
	for _, unit := range []struct {
		expr string
		size time.Duration
	}{
		{"24*" + timePkg + ".Hour", 24 * time.Hour},
		{timePkg + ".Hour", time.Hour},
		{timePkg + ".Minute", time.Minute},
		{timePkg + ".Second", time.Second},
		{timePkg + ".Millisecond", time.Millisecond},
		{timePkg + ".Microsecond", time.Microsecond},
		{timePkg + ".Nanosecond", time.Nanosecond},
	} {
		n := v / unit.size
		if n == 0 {
			continue
		}
		v -= n * unit.size

		if n == 1 && !strings.HasPrefix(unit.expr, "24*") {
			terms = append(terms, unit.expr)
			continue
		}
		terms = append(terms, fmt.Sprintf("%d*%s", n, unit.expr))
	}

	return strings.Join(terms, " + ")
}
